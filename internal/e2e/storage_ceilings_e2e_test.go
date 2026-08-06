package e2e_test

// storage_ceilings_e2e_test.go — a saturated connection pool must fail an entity
// write FAST, with a retryable 503 STORAGE_UNAVAILABLE, on both entry points.
//
// These are fault tests: they assert the outcome (one 503, quickly, carrying the
// right code) rather than a precise interleave. They run on their own
// one-connection app — against the package's shared stack they could not
// saturate anything in isolation, and a saturation scenario there would stall
// every other test for a full acquire timeout.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
)

const (
	storageCeilingSample = `{"name":"pool-hold","amount":1,"status":"new"}`
	// Short enough that a saturated write reports in well under the test's
	// fail-fast budget, long enough not to fire on ordinary scheduling jitter.
	storageCeilingAcquireTimeout = "500ms"
)

// storageCeilingModel derives a per-test model name. Each test here stands up
// its own app, but they all share the package's Postgres container, so a fixed
// name would collide on the second import (MODEL_ALREADY_LOCKED).
func storageCeilingModel(t *testing.T) string {
	t.Helper()
	return "pool-hold-" + strings.ToLower(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, t.Name()))
}

// holdHarness is a one-connection stack whose gate criterion parks the server's
// request goroutine with the transaction — and therefore the pool's only
// connection — in hand, until the test releases it.
type holdHarness struct {
	*callbackHarness
	model   string
	entered chan struct{} // closed by the criterion once it is holding
	release chan struct{}
	done    chan createEntityResult
}

// newSaturatedPoolHarness builds the stack and imports the model whose single
// automated transition is gated by the blocking criterion.
func newSaturatedPoolHarness(t *testing.T) *holdHarness {
	t.Helper()
	svc := localproc.New()
	h := newTinyPoolHarnessConfigured(t, 1, func(cfg *app.Config) {
		// Read by the postgres plugin's own getenv at factory-open time, which
		// happens inside app.New — i.e. after this mutator runs.
		t.Setenv("CYODA_POSTGRES_ACQUIRE_TIMEOUT", storageCeilingAcquireTimeout)
		// In-process dispatch, so the criterion below runs on the SERVER's
		// request goroutine with the transaction open.
		cfg.ExternalProcessing = svc
		// The scan loop would queue behind the held connection for the whole
		// test and log acquire failures unrelated to what is under test.
		cfg.Scheduler.Enabled = false
	})

	hh := &holdHarness{
		callbackHarness: h,
		model:           storageCeilingModel(t),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
		done:            make(chan createEntityResult, 1),
	}

	var once sync.Once
	criterion := hh.model + "-crit"
	svc.RegisterCriteria(criterion, func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
		once.Do(func() { close(hh.entered) })
		<-hh.release
		return false, nil // no match: the holder's own write is not what is asserted
	})
	h.setupModelSampleWithWorkflow(t, hh.model, storageCeilingSample,
		txLifeGateWF(hh.model+"-wf", criterion))
	return hh
}

// hold drives one create into the gate criterion and returns once that criterion
// is parked holding the only connection. The returned func releases it and waits
// for the holding request to finish.
func (hh *holdHarness) hold(t *testing.T) func() {
	t.Helper()
	// Seed the cached bearer on the test goroutine, and warm the model cache, so
	// neither the holder nor the saturated request below needs a connection
	// before it reaches Begin.
	hh.token(t)

	go func() { hh.done <- hh.CreateEntityRaw(hh.model, 1, storageCeilingSample) }()

	select {
	case <-hh.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the gate criterion never ran; the pool was never saturated")
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			close(hh.release)
			select {
			case <-hh.done:
			case <-time.After(30 * time.Second):
				t.Error("the holding request never returned")
			}
		})
	}
}

// TestE2E_SaturatedPool_WriteReturns503 is the HTTP half: the write fails fast
// with a retryable 503 STORAGE_UNAVAILABLE instead of queueing behind the pool.
func TestE2E_SaturatedPool_WriteReturns503(t *testing.T) {
	h := newSaturatedPoolHarness(t)
	release := h.hold(t)
	defer release()

	start := time.Now()
	_, status, body := h.CreateEntity(t, h.model, 1, storageCeilingSample)
	elapsed := time.Since(start)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", status, body)
	}
	assertStorageUnavailableProblem(t, body)
	if elapsed > 3*time.Second {
		t.Fatalf("queued for %v instead of failing fast", elapsed)
	}
	t.Logf("saturated-pool write returned 503 after %v; body: %s", elapsed, body)
}

// assertStorageUnavailableProblem checks the RFC 9457 body carries the error
// code and the retryable flag, and that the client-facing detail stays generic —
// the cause is infrastructure and belongs in the log, not the response.
func assertStorageUnavailableProblem(t *testing.T, body string) {
	t.Helper()
	var pd struct {
		Status int            `json:"status"`
		Detail string         `json:"detail"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body: %s", err, body)
	}
	if code, _ := pd.Props["errorCode"].(string); code != "STORAGE_UNAVAILABLE" {
		t.Errorf("errorCode = %q, want STORAGE_UNAVAILABLE; body: %s", code, body)
	}
	if retryable, _ := pd.Props["retryable"].(bool); !retryable {
		t.Errorf("retryable is not set; body: %s", body)
	}
	for _, leak := range []string{"postgres://", "password", "dbname=", "127.0.0.1"} {
		if strings.Contains(strings.ToLower(pd.Detail), leak) {
			t.Errorf("client-facing detail leaks infrastructure (%q): %s", leak, pd.Detail)
		}
	}
}

// TestE2E_SaturatedPool_GRPCEnvelope is the gRPC half. HTTP and gRPC are
// separate entry points and both must be covered. Over gRPC the envelope's code
// field is the generic class CLIENT_ERROR; the domain code is carried in the
// message, which is the established convention for this surface.
func TestE2E_SaturatedPool_GRPCEnvelope(t *testing.T) {
	h := newSaturatedPoolHarness(t)
	release := h.hold(t)
	defer release()

	start := time.Now()
	env, err := h.createEntityGRPC(h.model, 1, storageCeilingSample)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("gRPC create: %v", err)
	}

	if env.Success {
		t.Fatal("gRPC create succeeded on a saturated one-connection pool")
	}
	if env.Error == nil {
		t.Fatal("failed gRPC create carried no error envelope")
	}
	if env.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR (the envelope class)", env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, "STORAGE_UNAVAILABLE:") {
		t.Errorf("Error.Message = %q, want the STORAGE_UNAVAILABLE domain code", env.Error.Message)
	}
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Errorf("Error.Retryable = %v, want true", env.Error.Retryable)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("queued for %v instead of failing fast", elapsed)
	}
	t.Logf("saturated-pool gRPC create failed after %v: %s", elapsed, env.Error.Message)
}

// txEnvelope is the error-bearing subset of EntityTransactionResponse.
type txEnvelope struct {
	Success bool `json:"success"`
	Error   *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

// createEntityGRPC issues an EntityCreateRequest over the real gRPC entity API
// (the member's connection), unjoined.
func (h *callbackHarness) createEntityGRPC(model string, version int, payload string) (txEnvelope, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, err
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateRequest, map[string]any{
		"id":         "storage-ceiling-create",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		},
	})
	if err != nil {
		return txEnvelope{}, err
	}
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	respCE, err := client.EntityManage(h.grpcCtx(""), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}

func parseTxEnvelope(ce *cepb.CloudEvent) (txEnvelope, error) {
	_, payload, err := internalgrpc.ParseCloudEvent(ce)
	if err != nil {
		return txEnvelope{}, err
	}
	var env txEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return txEnvelope{}, errors.New("unmarshal EntityTransactionResponse: " + err.Error())
	}
	return env, nil
}
