package grpc

import (
	"context"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// TxTokenAttr is the CloudEvent extension attribute carrying the signed
// transaction routing token on an outbound processor/criteria calc request.
// The compute-node SDK echoes it as tx-token gRPC metadata / X-Tx-Token HTTP
// header on any callback into cyoda-go.
const TxTokenAttr = "cyodatxtoken"

func AttachTxToken(ce *cepb.CloudEvent, tok string) {
	if ce == nil || tok == "" {
		return
	}
	if ce.Attributes == nil {
		ce.Attributes = make(map[string]*cepb.CloudEvent_CloudEventAttributeValue)
	}
	ce.Attributes[TxTokenAttr] = &cepb.CloudEvent_CloudEventAttributeValue{
		Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: tok},
	}
}

func TxTokenFromCloudEvent(ce *cepb.CloudEvent) string {
	if ce == nil || ce.Attributes == nil {
		return ""
	}
	v, ok := ce.Attributes[TxTokenAttr]
	if !ok {
		return ""
	}
	return v.GetCeString()
}

type txTokenCtxKey struct{}

func WithTxToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, txTokenCtxKey{}, tok)
}

func TxTokenFromContext(ctx context.Context) string {
	tok, _ := ctx.Value(txTokenCtxKey{}).(string)
	return tok
}
