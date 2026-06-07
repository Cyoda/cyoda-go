package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
	"github.com/cyoda-platform/cyoda-go/internal/domain/audit"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/messaging"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// Server composes domain handlers with an Unimplemented fallback for any
// methods that are not yet wired to a real handler.
type Server struct {
	*Unimplemented

	Entity    *entity.Handler
	Model     *model.Handler
	Workflow  *workflow.Handler
	Search    *search.Handler
	Audit     *audit.Handler
	Messaging *messaging.Handler
	Account   *account.Handler
}

// NewServer returns a Server with the Unimplemented fallback initialised.
// Domain handler fields are nil by default; callers set them after construction.
func NewServer() *Server {
	return &Server{
		Unimplemented: &Unimplemented{},
	}
}

// ---------------------------------------------------------------------------
// Entity delegation (15 methods)
// ---------------------------------------------------------------------------

func (s *Server) GetEntityStatistics(w http.ResponseWriter, r *http.Request, params genapi.GetEntityStatisticsParams) {
	if s.Entity != nil {
		s.Entity.GetEntityStatistics(w, r, params)
		return
	}
	s.Unimplemented.GetEntityStatistics(w, r, params)
}

func (s *Server) GetEntityStatisticsByState(w http.ResponseWriter, r *http.Request, params genapi.GetEntityStatisticsByStateParams) {
	if s.Entity != nil {
		s.Entity.GetEntityStatisticsByState(w, r, params)
		return
	}
	s.Unimplemented.GetEntityStatisticsByState(w, r, params)
}

func (s *Server) GetEntityStatisticsByStateForModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetEntityStatisticsByStateForModelParams) {
	if s.Entity != nil {
		s.Entity.GetEntityStatisticsByStateForModel(w, r, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.GetEntityStatisticsByStateForModel(w, r, entityName, modelVersion, params)
}

func (s *Server) GetEntityStatisticsForModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetEntityStatisticsForModelParams) {
	if s.Entity != nil {
		s.Entity.GetEntityStatisticsForModel(w, r, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.GetEntityStatisticsForModel(w, r, entityName, modelVersion, params)
}

func (s *Server) DeleteSingleEntity(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID) {
	if s.Entity != nil {
		s.Entity.DeleteSingleEntity(w, r, entityId)
		return
	}
	s.Unimplemented.DeleteSingleEntity(w, r, entityId)
}

func (s *Server) GetOneEntity(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetOneEntityParams) {
	if s.Entity != nil {
		s.Entity.GetOneEntity(w, r, entityId, params)
		return
	}
	s.Unimplemented.GetOneEntity(w, r, entityId, params)
}

func (s *Server) GetEntityChangesMetadata(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetEntityChangesMetadataParams) {
	if s.Entity != nil {
		s.Entity.GetEntityChangesMetadata(w, r, entityId, params)
		return
	}
	s.Unimplemented.GetEntityChangesMetadata(w, r, entityId, params)
}

// GetEntityTransitions and FetchEntityTransitions are routed directly in app/app.go
// before the generated API mux — these delegation methods satisfy ServerInterface
// but are never reached in production.
// TODO(#21-future-cleanup, see ADR 0001 / Task 5.1): consolidate the
// transitions handler with the generated ServerInterface dispatch.
// Currently the real handlers are mounted directly via app.go's outer mux
// (which routes BEFORE the generated dispatch fires); these stubs are
// unreachable in production.
func (s *Server) GetEntityTransitions(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetEntityTransitionsParams) {
	s.Unimplemented.GetEntityTransitions(w, r, entityId, params)
}

func (s *Server) FetchEntityTransitions(w http.ResponseWriter, r *http.Request, params genapi.FetchEntityTransitionsParams) {
	s.Unimplemented.FetchEntityTransitions(w, r, params)
}

func (s *Server) DeleteEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.DeleteEntitiesParams) {
	if s.Entity != nil {
		s.Entity.DeleteEntities(w, r, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.DeleteEntities(w, r, entityName, modelVersion, params)
}

func (s *Server) GetAllEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetAllEntitiesParams) {
	if s.Entity != nil {
		s.Entity.GetAllEntities(w, r, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.GetAllEntities(w, r, entityName, modelVersion, params)
}

func (s *Server) CreateCollection(w http.ResponseWriter, r *http.Request, format genapi.CreateCollectionParamsFormat, params genapi.CreateCollectionParams) {
	if s.Entity != nil {
		s.Entity.CreateCollection(w, r, format, params)
		return
	}
	s.Unimplemented.CreateCollection(w, r, format, params)
}

func (s *Server) UpdateCollection(w http.ResponseWriter, r *http.Request, format genapi.UpdateCollectionParamsFormat, params genapi.UpdateCollectionParams) {
	if s.Entity != nil {
		s.Entity.UpdateCollection(w, r, format, params)
		return
	}
	s.Unimplemented.UpdateCollection(w, r, format, params)
}

func (s *Server) UpdateSingleWithLoopback(w http.ResponseWriter, r *http.Request, format genapi.UpdateSingleWithLoopbackParamsFormat, entityId openapi_types.UUID, params genapi.UpdateSingleWithLoopbackParams) {
	if s.Entity != nil {
		s.Entity.UpdateSingleWithLoopback(w, r, format, entityId, params)
		return
	}
	s.Unimplemented.UpdateSingleWithLoopback(w, r, format, entityId, params)
}

func (s *Server) UpdateSingle(w http.ResponseWriter, r *http.Request, format genapi.UpdateSingleParamsFormat, entityId openapi_types.UUID, transition string, params genapi.UpdateSingleParams) {
	if s.Entity != nil {
		s.Entity.UpdateSingle(w, r, format, entityId, transition, params)
		return
	}
	s.Unimplemented.UpdateSingle(w, r, format, entityId, transition, params)
}

func (s *Server) Create(w http.ResponseWriter, r *http.Request, format genapi.CreateParamsFormat, entityName string, modelVersion int32, params genapi.CreateParams) {
	if s.Entity != nil {
		s.Entity.Create(w, r, format, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.Create(w, r, format, entityName, modelVersion, params)
}

// ---------------------------------------------------------------------------
// Model delegation (8 methods)
// ---------------------------------------------------------------------------

func (s *Server) GetAvailableEntityModels(w http.ResponseWriter, r *http.Request) {
	if s.Model != nil {
		s.Model.GetAvailableEntityModels(w, r)
		return
	}
	s.Unimplemented.GetAvailableEntityModels(w, r)
}

func (s *Server) ExportMetadata(w http.ResponseWriter, r *http.Request, converter genapi.ExportMetadataParamsConverter, entityName string, modelVersion int32) {
	if s.Model != nil {
		s.Model.ExportMetadata(w, r, converter, entityName, modelVersion)
		return
	}
	s.Unimplemented.ExportMetadata(w, r, converter, entityName, modelVersion)
}

func (s *Server) ImportEntityModel(w http.ResponseWriter, r *http.Request, dataFormat genapi.ImportEntityModelParamsDataFormat, converter genapi.ImportEntityModelParamsConverter, entityName string, modelVersion int32) {
	if s.Model != nil {
		s.Model.ImportEntityModel(w, r, dataFormat, converter, entityName, modelVersion)
		return
	}
	s.Unimplemented.ImportEntityModel(w, r, dataFormat, converter, entityName, modelVersion)
}

func (s *Server) DeleteEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	if s.Model != nil {
		s.Model.DeleteEntityModel(w, r, entityName, modelVersion)
		return
	}
	s.Unimplemented.DeleteEntityModel(w, r, entityName, modelVersion)
}

func (s *Server) SetEntityModelChangeLevel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, changeLevel genapi.SetEntityModelChangeLevelParamsChangeLevel) {
	if s.Model != nil {
		s.Model.SetEntityModelChangeLevel(w, r, entityName, modelVersion, changeLevel)
		return
	}
	s.Unimplemented.SetEntityModelChangeLevel(w, r, entityName, modelVersion, changeLevel)
}

func (s *Server) LockEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	if s.Model != nil {
		s.Model.LockEntityModel(w, r, entityName, modelVersion)
		return
	}
	s.Unimplemented.LockEntityModel(w, r, entityName, modelVersion)
}

func (s *Server) UnlockEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	if s.Model != nil {
		s.Model.UnlockEntityModel(w, r, entityName, modelVersion)
		return
	}
	s.Unimplemented.UnlockEntityModel(w, r, entityName, modelVersion)
}

func (s *Server) ValidateEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	if s.Model != nil {
		s.Model.ValidateEntityModel(w, r, entityName, modelVersion)
		return
	}
	s.Unimplemented.ValidateEntityModel(w, r, entityName, modelVersion)
}

// ---------------------------------------------------------------------------
// Workflow delegation (2 methods)
// ---------------------------------------------------------------------------

func (s *Server) ExportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	if s.Workflow != nil {
		s.Workflow.ExportEntityModelWorkflow(w, r, entityName, modelVersion)
		return
	}
	s.Unimplemented.ExportEntityModelWorkflow(w, r, entityName, modelVersion)
}

func (s *Server) ImportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	if s.Workflow != nil {
		s.Workflow.ImportEntityModelWorkflow(w, r, entityName, modelVersion)
		return
	}
	s.Unimplemented.ImportEntityModelWorkflow(w, r, entityName, modelVersion)
}

// ---------------------------------------------------------------------------
// Search delegation (5 methods)
// ---------------------------------------------------------------------------

func (s *Server) SubmitAsyncSearchJob(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.SubmitAsyncSearchJobParams) {
	if s.Search != nil {
		s.Search.SubmitAsyncSearchJob(w, r, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.SubmitAsyncSearchJob(w, r, entityName, modelVersion, params)
}

func (s *Server) GetAsyncSearchResults(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID, params genapi.GetAsyncSearchResultsParams) {
	if s.Search != nil {
		s.Search.GetAsyncSearchResults(w, r, jobId, params)
		return
	}
	s.Unimplemented.GetAsyncSearchResults(w, r, jobId, params)
}

func (s *Server) CancelAsyncSearch(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID) {
	if s.Search != nil {
		s.Search.CancelAsyncSearch(w, r, jobId)
		return
	}
	s.Unimplemented.CancelAsyncSearch(w, r, jobId)
}

func (s *Server) GetAsyncSearchStatus(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID) {
	if s.Search != nil {
		s.Search.GetAsyncSearchStatus(w, r, jobId)
		return
	}
	s.Unimplemented.GetAsyncSearchStatus(w, r, jobId)
}

func (s *Server) SearchEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.SearchEntitiesParams) {
	if s.Search != nil {
		s.Search.SearchEntities(w, r, entityName, modelVersion, params)
		return
	}
	s.Unimplemented.SearchEntities(w, r, entityName, modelVersion, params)
}

// ---------------------------------------------------------------------------
// Audit delegation (2 methods)
// ---------------------------------------------------------------------------

func (s *Server) SearchEntityAuditEvents(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.SearchEntityAuditEventsParams) {
	if s.Audit != nil {
		s.Audit.SearchEntityAuditEvents(w, r, entityId, params)
		return
	}
	s.Unimplemented.SearchEntityAuditEvents(w, r, entityId, params)
}

func (s *Server) GetStateMachineFinishedEvent(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, transactionId openapi_types.UUID) {
	if s.Audit != nil {
		s.Audit.GetStateMachineFinishedEvent(w, r, entityId, transactionId)
		return
	}
	s.Unimplemented.GetStateMachineFinishedEvent(w, r, entityId, transactionId)
}

// ---------------------------------------------------------------------------
// Messaging delegation (4 methods)
// ---------------------------------------------------------------------------

func (s *Server) DeleteMessages(w http.ResponseWriter, r *http.Request, params genapi.DeleteMessagesParams) {
	if s.Messaging != nil {
		s.Messaging.DeleteMessages(w, r, params)
		return
	}
	s.Unimplemented.DeleteMessages(w, r, params)
}

func (s *Server) NewMessage(w http.ResponseWriter, r *http.Request, subject string, params genapi.NewMessageParams) {
	if s.Messaging != nil {
		s.Messaging.NewMessage(w, r, subject, params)
		return
	}
	s.Unimplemented.NewMessage(w, r, subject, params)
}

func (s *Server) DeleteMessage(w http.ResponseWriter, r *http.Request, messageId string) {
	if s.Messaging != nil {
		s.Messaging.DeleteMessage(w, r, messageId)
		return
	}
	s.Unimplemented.DeleteMessage(w, r, messageId)
}

func (s *Server) GetMessage(w http.ResponseWriter, r *http.Request, messageId string) {
	if s.Messaging != nil {
		s.Messaging.GetMessage(w, r, messageId)
		return
	}
	s.Unimplemented.GetMessage(w, r, messageId)
}

// ---------------------------------------------------------------------------
// Account delegation (24 methods)
// ---------------------------------------------------------------------------

func (s *Server) AccountGet(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.AccountGet(w, r)
		return
	}
	s.Unimplemented.AccountGet(w, r)
}

func (s *Server) AccountSubscriptionsGet(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.AccountSubscriptionsGet(w, r)
		return
	}
	s.Unimplemented.AccountSubscriptionsGet(w, r)
}

func (s *Server) ListTechnicalUsers(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.ListTechnicalUsers(w, r)
		return
	}
	s.Unimplemented.ListTechnicalUsers(w, r)
}

func (s *Server) CreateTechnicalUser(w http.ResponseWriter, r *http.Request, params genapi.CreateTechnicalUserParams) {
	if s.Account != nil {
		s.Account.CreateTechnicalUser(w, r, params)
		return
	}
	s.Unimplemented.CreateTechnicalUser(w, r, params)
}

func (s *Server) DeleteTechnicalUser(w http.ResponseWriter, r *http.Request, clientId string) {
	if s.Account != nil {
		s.Account.DeleteTechnicalUser(w, r, clientId)
		return
	}
	s.Unimplemented.DeleteTechnicalUser(w, r, clientId)
}

func (s *Server) ResetTechnicalUserSecret(w http.ResponseWriter, r *http.Request, clientId string) {
	if s.Account != nil {
		s.Account.ResetTechnicalUserSecret(w, r, clientId)
		return
	}
	s.Unimplemented.ResetTechnicalUserSecret(w, r, clientId)
}

func (s *Server) GetTechnicalUserToken(w http.ResponseWriter, r *http.Request, params genapi.GetTechnicalUserTokenParams) {
	if s.Account != nil {
		s.Account.GetTechnicalUserToken(w, r, params)
		return
	}
	s.Unimplemented.GetTechnicalUserToken(w, r, params)
}

func (s *Server) IssueJwtKeyPair(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.IssueJwtKeyPair(w, r)
		return
	}
	s.Unimplemented.IssueJwtKeyPair(w, r)
}

func (s *Server) GetCurrentJwtKeyPair(w http.ResponseWriter, r *http.Request, params genapi.GetCurrentJwtKeyPairParams) {
	if s.Account != nil {
		s.Account.GetCurrentJwtKeyPair(w, r, params)
		return
	}
	s.Unimplemented.GetCurrentJwtKeyPair(w, r, params)
}

func (s *Server) DeleteJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if s.Account != nil {
		s.Account.DeleteJwtKeyPair(w, r, keyId)
		return
	}
	s.Unimplemented.DeleteJwtKeyPair(w, r, keyId)
}

func (s *Server) InvalidateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if s.Account != nil {
		s.Account.InvalidateJwtKeyPair(w, r, keyId)
		return
	}
	s.Unimplemented.InvalidateJwtKeyPair(w, r, keyId)
}

func (s *Server) ReactivateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if s.Account != nil {
		s.Account.ReactivateJwtKeyPair(w, r, keyId)
		return
	}
	s.Unimplemented.ReactivateJwtKeyPair(w, r, keyId)
}

func (s *Server) ListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.ListTrustedKeys(w, r)
		return
	}
	s.Unimplemented.ListTrustedKeys(w, r)
}

func (s *Server) RegisterTrustedKey(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.RegisterTrustedKey(w, r)
		return
	}
	s.Unimplemented.RegisterTrustedKey(w, r)
}

func (s *Server) DeleteTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if s.Account != nil {
		s.Account.DeleteTrustedKey(w, r, keyId)
		return
	}
	s.Unimplemented.DeleteTrustedKey(w, r, keyId)
}

func (s *Server) InvalidateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if s.Account != nil {
		s.Account.InvalidateTrustedKey(w, r, keyId)
		return
	}
	s.Unimplemented.InvalidateTrustedKey(w, r, keyId)
}

func (s *Server) ReactivateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if s.Account != nil {
		s.Account.ReactivateTrustedKey(w, r, keyId)
		return
	}
	s.Unimplemented.ReactivateTrustedKey(w, r, keyId)
}

func (s *Server) ListOidcProviders(w http.ResponseWriter, r *http.Request, params genapi.ListOidcProvidersParams) {
	if s.Account != nil {
		s.Account.ListOidcProviders(w, r, params)
		return
	}
	s.Unimplemented.ListOidcProviders(w, r, params)
}

func (s *Server) RegisterOidcProvider(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.RegisterOidcProvider(w, r)
		return
	}
	s.Unimplemented.RegisterOidcProvider(w, r)
}

func (s *Server) ReloadOidcProviders(w http.ResponseWriter, r *http.Request) {
	if s.Account != nil {
		s.Account.ReloadOidcProviders(w, r)
		return
	}
	s.Unimplemented.ReloadOidcProviders(w, r)
}

func (s *Server) DeleteOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if s.Account != nil {
		s.Account.DeleteOidcProvider(w, r, id)
		return
	}
	s.Unimplemented.DeleteOidcProvider(w, r, id)
}

func (s *Server) UpdateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if s.Account != nil {
		s.Account.UpdateOidcProvider(w, r, id)
		return
	}
	s.Unimplemented.UpdateOidcProvider(w, r, id)
}

func (s *Server) InvalidateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if s.Account != nil {
		s.Account.InvalidateOidcProvider(w, r, id)
		return
	}
	s.Unimplemented.InvalidateOidcProvider(w, r, id)
}

func (s *Server) ReactivateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if s.Account != nil {
		s.Account.ReactivateOidcProvider(w, r, id)
		return
	}
	s.Unimplemented.ReactivateOidcProvider(w, r, id)
}
