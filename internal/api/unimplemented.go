package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Unimplemented satisfies genapi.ServerInterface with every method returning
// HTTP 501 Not Implemented using RFC 9457 problem+json.
type Unimplemented struct{}

func (u *Unimplemented) stub(w http.ResponseWriter, r *http.Request) {
	common.WriteError(w, r, common.Operational(http.StatusNotImplemented, common.ErrCodeNotImplemented, "not yet implemented"))
}

func (u *Unimplemented) AccountGet(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) AccountSubscriptionsGet(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) SearchEntityAuditEvents(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.SearchEntityAuditEventsParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetStateMachineFinishedEvent(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, transactionId openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) ListTechnicalUsers(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) CreateTechnicalUser(w http.ResponseWriter, r *http.Request, params genapi.CreateTechnicalUserParams) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteTechnicalUser(w http.ResponseWriter, r *http.Request, clientId string) {
	u.stub(w, r)
}

func (u *Unimplemented) ResetTechnicalUserSecret(w http.ResponseWriter, r *http.Request, clientId string) {
	u.stub(w, r)
}

func (u *Unimplemented) GetEntityStatistics(w http.ResponseWriter, r *http.Request, params genapi.GetEntityStatisticsParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetEntityStatisticsByState(w http.ResponseWriter, r *http.Request, params genapi.GetEntityStatisticsByStateParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetEntityStatisticsByStateForModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetEntityStatisticsByStateForModelParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetEntityStatisticsForModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetEntityStatisticsForModelParams) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteSingleEntity(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) GetOneEntity(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetOneEntityParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetEntityChangesMetadata(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetEntityChangesMetadataParams) {
	u.stub(w, r)
}

// GetEntityTransitions is never called — the real handler is mounted directly
// on the outer mux in app/app.go before the generated API handler.
// TODO(future-cleanup): consolidate transitions handler with the generated ServerInterface dispatch.
func (u *Unimplemented) GetEntityTransitions(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetEntityTransitionsParams) {
	u.stub(w, r)
}

// FetchEntityTransitions is never called — the real handler is mounted directly
// on the outer mux in app/app.go before the generated API handler.
// TODO(future-cleanup): consolidate transitions handler with the generated ServerInterface dispatch.
func (u *Unimplemented) FetchEntityTransitions(w http.ResponseWriter, r *http.Request, params genapi.FetchEntityTransitionsParams) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.DeleteEntitiesParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetAllEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetAllEntitiesParams) {
	u.stub(w, r)
}

func (u *Unimplemented) CreateCollection(w http.ResponseWriter, r *http.Request, format genapi.CreateCollectionParamsFormat, params genapi.CreateCollectionParams) {
	u.stub(w, r)
}

func (u *Unimplemented) UpdateCollection(w http.ResponseWriter, r *http.Request, format genapi.UpdateCollectionParamsFormat, params genapi.UpdateCollectionParams) {
	u.stub(w, r)
}

func (u *Unimplemented) UpdateSingleWithLoopback(w http.ResponseWriter, r *http.Request, format genapi.UpdateSingleWithLoopbackParamsFormat, entityId openapi_types.UUID, params genapi.UpdateSingleWithLoopbackParams) {
	u.stub(w, r)
}

func (u *Unimplemented) UpdateSingle(w http.ResponseWriter, r *http.Request, format genapi.UpdateSingleParamsFormat, entityId openapi_types.UUID, transition string, params genapi.UpdateSingleParams) {
	u.stub(w, r)
}

func (u *Unimplemented) Create(w http.ResponseWriter, r *http.Request, format genapi.CreateParamsFormat, entityName string, modelVersion int32, params genapi.CreateParams) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteMessages(w http.ResponseWriter, r *http.Request, params genapi.DeleteMessagesParams) {
	u.stub(w, r)
}

func (u *Unimplemented) NewMessage(w http.ResponseWriter, r *http.Request, subject string, params genapi.NewMessageParams) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteMessage(w http.ResponseWriter, r *http.Request, messageId string) {
	u.stub(w, r)
}

func (u *Unimplemented) GetMessage(w http.ResponseWriter, r *http.Request, messageId string) {
	u.stub(w, r)
}

func (u *Unimplemented) GetAvailableEntityModels(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) ExportMetadata(w http.ResponseWriter, r *http.Request, converter genapi.ExportMetadataParamsConverter, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) ImportEntityModel(w http.ResponseWriter, r *http.Request, dataFormat genapi.ImportEntityModelParamsDataFormat, converter genapi.ImportEntityModelParamsConverter, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) SetEntityModelChangeLevel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, changeLevel genapi.SetEntityModelChangeLevelParamsChangeLevel) {
	u.stub(w, r)
}

func (u *Unimplemented) LockEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) UnlockEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) ValidateEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) ExportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) ImportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	u.stub(w, r)
}

func (u *Unimplemented) IssueJwtKeyPair(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) GetCurrentJwtKeyPair(w http.ResponseWriter, r *http.Request, params genapi.GetCurrentJwtKeyPairParams) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	u.stub(w, r)
}

func (u *Unimplemented) InvalidateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	u.stub(w, r)
}

func (u *Unimplemented) ReactivateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	u.stub(w, r)
}

func (u *Unimplemented) ListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) RegisterTrustedKey(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	u.stub(w, r)
}

func (u *Unimplemented) InvalidateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	u.stub(w, r)
}

func (u *Unimplemented) ReactivateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	u.stub(w, r)
}

func (u *Unimplemented) ListOidcProviders(w http.ResponseWriter, r *http.Request, params genapi.ListOidcProvidersParams) {
	u.stub(w, r)
}

func (u *Unimplemented) RegisterOidcProvider(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) ReloadOidcProviders(w http.ResponseWriter, r *http.Request) {
	u.stub(w, r)
}

func (u *Unimplemented) DeleteOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) UpdateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) InvalidateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) ReactivateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) GetTechnicalUserToken(w http.ResponseWriter, r *http.Request, params genapi.GetTechnicalUserTokenParams) {
	u.stub(w, r)
}

func (u *Unimplemented) SubmitAsyncSearchJob(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.SubmitAsyncSearchJobParams) {
	u.stub(w, r)
}

func (u *Unimplemented) GetAsyncSearchResults(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID, params genapi.GetAsyncSearchResultsParams) {
	u.stub(w, r)
}

func (u *Unimplemented) CancelAsyncSearch(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) GetAsyncSearchStatus(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID) {
	u.stub(w, r)
}

func (u *Unimplemented) SearchEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.SearchEntitiesParams) {
	u.stub(w, r)
}
