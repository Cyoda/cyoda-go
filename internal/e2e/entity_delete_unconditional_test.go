package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

type deleteResp struct {
	EntityModelClassID string `json:"entityModelClassId"`
	DeleteResult       struct {
		IDToError                map[string]string `json:"idToError"`
		NumberOfEntitites        int               `json:"numberOfEntitites"`
		NumberOfEntititesRemoved int               `json:"numberOfEntititesRemoved"`
	} `json:"deleteResult"`
	IDs []string `json:"ids"`
}

func deleteModelEntities(t *testing.T, model, rawQuery string) (int, deleteResp, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/1", model)
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	var out deleteResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode delete response: %v: %s", err, body)
		}
	}
	return resp.StatusCode, out, body
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func entityExists(t *testing.T, id string) bool {
	t.Helper()
	resp := doAuth(t, http.MethodGet, "/api/entity/"+id, "")
	readBody(t, resp)
	return resp.StatusCode == http.StatusOK
}

// An empty-body delete with pointInTime selects the entities that existed
// at the instant and deletes their current rows; entities created after
// the instant survive; an entity selected at the instant but already gone
// is reported per id. verbose lists every attempted id.
func TestDeleteEntities_Unconditional_PointInTime(t *testing.T) {
	const model = "e2e-deluncond-pit"
	importModelWithSample(t, model, 1, `{"n":0}`)
	lockModelE2E(t, model, 1)

	a := createEntityE2E(t, model, 1, `{"n":1}`)
	b := createEntityE2E(t, model, 1, `{"n":2}`)
	tB := latestChangeTimeE2E(t, b)
	time.Sleep(10 * time.Millisecond)
	c := createEntityE2E(t, model, 1, `{"n":3}`)
	tC := latestChangeTimeE2E(t, c)
	instant := midpointBetweenE2E(t, tB, tC) // server-clock boundary between b and c

	// b is selected at the instant but gone by the time the delete runs.
	if resp := doAuth(t, http.MethodDelete, "/api/entity/"+b, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete b: %d: %s", resp.StatusCode, readBody(t, resp))
	}

	status, out, body := deleteModelEntities(t, model, "pointInTime="+instant+"&verbose=true")
	if status != http.StatusOK {
		t.Fatalf("delete as-at: %d: %s", status, body)
	}
	if out.DeleteResult.NumberOfEntitites != 2 || out.DeleteResult.NumberOfEntititesRemoved != 1 {
		t.Errorf("matched/removed = %d/%d, want 2/1: %s", out.DeleteResult.NumberOfEntitites, out.DeleteResult.NumberOfEntititesRemoved, body)
	}
	if _, ok := out.DeleteResult.IDToError[b]; !ok {
		t.Errorf("idToError = %v, want an entry for the already-gone id %s", out.DeleteResult.IDToError, b)
	}
	if got, want := sortedStrings(out.IDs), sortedStrings([]string{a, b}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want the attempted set %v", got, want)
	}
	if entityExists(t, a) {
		t.Error("a existed at the instant and must be gone")
	}
	if !entityExists(t, c) {
		t.Error("c was created after the instant and must survive")
	}

	t.Run("future instant is refused", func(t *testing.T) {
		// An instant later than the consistency time could still change, so the
		// delete does not select by it: nothing is removed.
		d := createEntityE2E(t, model, 1, `{"n":4}`)
		status, _, body := deleteModelEntities(t, model, "pointInTime=2099-01-01T00:00:00Z&verbose=true")
		if status != http.StatusBadRequest || !strings.Contains(body, "POINT_IN_TIME_AFTER_CONSISTENCY_TIME") {
			t.Fatalf("delete as-at future: %d: %s, want 400 POINT_IN_TIME_AFTER_CONSISTENCY_TIME", status, body)
		}
		if !entityExists(t, c) || !entityExists(t, d) {
			t.Error("a refused delete must remove nothing")
		}
	})
}

func TestDeleteEntities_Unconditional_Verbose_ListsAttemptedIDs(t *testing.T) {
	const model = "e2e-deluncond-verbose"
	importModelWithSample(t, model, 1, `{"n":0}`)
	lockModelE2E(t, model, 1)
	ids := []string{
		createEntityE2E(t, model, 1, `{"n":1}`),
		createEntityE2E(t, model, 1, `{"n":2}`),
		createEntityE2E(t, model, 1, `{"n":3}`),
	}

	status, out, body := deleteModelEntities(t, model, "verbose=true")
	if status != http.StatusOK {
		t.Fatalf("delete verbose: %d: %s", status, body)
	}
	if out.DeleteResult.NumberOfEntitites != 3 || out.DeleteResult.NumberOfEntititesRemoved != 3 {
		t.Errorf("matched/removed = %d/%d, want 3/3: %s", out.DeleteResult.NumberOfEntitites, out.DeleteResult.NumberOfEntititesRemoved, body)
	}
	if got, want := sortedStrings(out.IDs), sortedStrings(ids); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", got, want)
	}

	t.Run("without verbose the ids field is absent", func(t *testing.T) {
		createEntityE2E(t, model, 1, `{"n":9}`)
		status, _, body := deleteModelEntities(t, model, "")
		if status != http.StatusOK {
			t.Fatalf("plain delete: %d: %s", status, body)
		}
		if strings.Contains(body, `"ids"`) {
			t.Errorf("plain delete must not carry ids: %s", body)
		}
	})
}

func TestDeleteEntities_Unconditional_PointInTime_ModelNotFound(t *testing.T) {
	resp := doAuth(t, http.MethodDelete, "/api/entity/e2e-deluncond-nosuch/1?pointInTime=2030-01-01T00:00:00Z", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}

func TestDeleteEntities_Unconditional_MalformedPointInTime_400(t *testing.T) {
	const model = "e2e-deluncond-badpit"
	importModelWithSample(t, model, 1, `{"n":0}`)
	lockModelE2E(t, model, 1)
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?pointInTime=not-a-time", model), "")
	commontest.ExpectErrorCode(t, resp, "BAD_REQUEST")
}
