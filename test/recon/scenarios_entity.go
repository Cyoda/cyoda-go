//go:build cyoda_recon

package recon

// EntityExclusions are fields excluded from entity response comparisons.
// Entity IDs and timestamps are generated dynamically and will differ between
// Cyoda-Go and Cyoda Cloud.
var EntityExclusions = []string{
	"/transactionId",
	"/entityIds",
}

// EntityEnvelopeExclusions are fields excluded from entity envelope comparisons.
var EntityEnvelopeExclusions = []string{
	"/meta/id",
	"/meta/creationDate",
	"/meta/lastUpdateTime",
	"/meta/transactionId",
}

// EntityStatsExclusions are fields excluded from entity stats comparisons.
var EntityStatsExclusions = []string{}

// entityScenarios returns the entity CRUD lifecycle and batch+stats flows.
func entityScenarios() []Scenario {
	return []Scenario{
		entityCRUDFlow(),
		batchAndStatsFlow(),
	}
}

// entityCRUDFlow exercises create, get, update, get, validate, delete, get-404.
func entityCRUDFlow() Scenario {
	return Scenario{
		Name: "Entity CRUD Lifecycle",
		Setup: func() map[string]string {
			return map[string]string{"model": uniqueName("EntityCRUD")}
		},
		Steps: []Step{
			// 1. Import model
			{
				Name:         "Import model",
				Method:       "POST",
				PathTemplate: "/model/import/JSON/SAMPLE_DATA/{model}/1",
				Body:         `{"name":"Alice","age":30}`,
				ExpectStatus: 200,
			},
			// 2. Lock model
			{
				Name:         "Lock model",
				Method:       "PUT",
				PathTemplate: "/model/{model}/1/lock",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 3. Create entity — capture entityId
			{
				Name:         "Create entity",
				Method:       "POST",
				PathTemplate: "/entity/JSON/{model}/1",
				Body:         `{"name":"Alice","age":30}`,
				ExpectStatus: 200,
				Exclusions:   EntityExclusions,
				Capture: map[string]string{
					"entityId": "0.entityIds.0",
				},
			},
			// 4. Direct search for entity
			{
				Name:         "Direct search for entity",
				Method:       "POST",
				PathTemplate: "/search/direct/{model}/1",
				Body:         `{"type": "simple", "jsonPath": "$.name", "operatorType": "EQUALS", "value": "Alice"}`,
				ExpectStatus: 200,
				Exclusions:   EntityEnvelopeExclusions,
			},
			// 5. Submit async search (Cloud returns bare UUID string)
			{
				Name:            "Submit async search",
				Method:          "POST",
				PathTemplate:    "/search/async/{model}/1",
				Body:            `{"type": "simple", "jsonPath": "$.name", "operatorType": "EQUALS", "value": "Alice"}`,
				ExpectStatus:    200,
				SkipBodyCompare: true, // both sides return bare UUID strings that will differ
				Capture: map[string]string{
					"searchJobId": "@this",
				},
			},
			// 6. Poll async search status
			{
				Name:         "Poll async search status",
				Method:       "GET",
				PathTemplate: "/search/async/{searchJobId}/status",
				ExpectStatus: 200,
				Exclusions:   []string{"/createTime", "/expirationDate", "/finishTime", "/calculationTimeMillis"},
			},
			// 7. Get async search results
			{
				Name:         "Get async search results",
				Method:       "GET",
				PathTemplate: "/search/async/{searchJobId}",
				ExpectStatus: 200,
				Exclusions: []string{
					"/page",
					"/content/meta/id",
					"/content/meta/creationDate",
					"/content/meta/lastUpdateTime",
					"/content/meta/transactionId",
				},
			},
			// 8. Cancel async search (already completed — expect 400)
			{
				Name:         "Cancel async search",
				Method:       "PUT",
				PathTemplate: "/search/async/{searchJobId}/cancel",
				ExpectStatus: 400,
				Exclusions:   []string{"/instance", "/detail", "/properties"},
			},
			// 9. Query audit after create
			{
				Name:         "Query audit after create",
				Method:       "GET",
				PathTemplate: "/audit/entity/{entityId}?eventType=EntityChange",
				ExpectStatus: 200,
				Exclusions: []string{
					"/items/utcTime",
					"/items/transactionId",
					"/items/entityId",
					"/items/meta",
					"/items/consistencyTime",
					"/items/actor",
					"/items/changes",
					"/items/entityModel",
					"/items/microsTime",
					"/items/system",
				},
			},
			// 6. Get entity by ID
			{
				Name:         "Get entity by ID",
				Method:       "GET",
				PathTemplate: "/entity/{entityId}",
				ExpectStatus: 200,
				Exclusions:   EntityEnvelopeExclusions,
			},
			// 7. Update entity
			{
				Name:         "Update entity",
				Method:       "PUT",
				PathTemplate: "/entity/JSON/{entityId}/UPDATE",
				Body:         `{"name":"Alice","age":31}`,
				ExpectStatus: 200,
				Exclusions:   EntityExclusions,
			},
			// 8. Get updated entity
			{
				Name:         "Get updated entity",
				Method:       "GET",
				PathTemplate: "/entity/{entityId}",
				ExpectStatus: 200,
				Exclusions:   EntityEnvelopeExclusions,
			},
			// 9. Delete entity
			{
				Name:         "Delete entity",
				Method:       "DELETE",
				PathTemplate: "/entity/{entityId}",
				ExpectStatus: 200,
				Exclusions:   []string{"/id", "/transactionId"},
			},
			// 10. Get deleted entity — expect 404
			{
				Name:         "Get deleted entity — expect 404",
				Method:       "GET",
				PathTemplate: "/entity/{entityId}",
				ExpectStatus: 404,
				Exclusions:   []string{"/instance", "/detail", "/properties/entityId"},
			},
		},
	}
}

// batchAndStatsFlow exercises batch create, stats endpoints, getAll, batch delete.
func batchAndStatsFlow() Scenario {
	return Scenario{
		Name: "Batch and Stats",
		Setup: func() map[string]string {
			return map[string]string{"model": uniqueName("BatchStats")}
		},
		Steps: []Step{
			// 1. Import model
			{
				Name:         "Import model",
				Method:       "POST",
				PathTemplate: "/model/import/JSON/SAMPLE_DATA/{model}/1",
				Body:         `{"name":"Alice","age":30}`,
				ExpectStatus: 200,
			},
			// 2. Lock model
			{
				Name:         "Lock model",
				Method:       "PUT",
				PathTemplate: "/model/{model}/1/lock",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 3. Batch create via CreateCollection
			{
				Name:   "Batch create entities",
				Method: "POST",
				// POST /entity/{format} — CreateCollection
				PathTemplate: "/entity/JSON",
				Body:         `[{"model":{"name":"{model}","version":1},"payload":"{\"name\":\"Alice\",\"age\":30}"},{"model":{"name":"{model}","version":1},"payload":"{\"name\":\"Bob\",\"age\":25}"}]`,
				ExpectStatus: 200,
				Exclusions:   EntityExclusions,
			},
			// 4. Get entity stats (global)
			{
				Name:         "Get entity statistics (global)",
				Method:       "GET",
				PathTemplate: "/entity/stats",
				ExpectStatus: 200,
				Exclusions:   EntityStatsExclusions,
			},
			// 5. Get entity stats for model
			{
				Name:         "Get entity statistics for model",
				Method:       "GET",
				PathTemplate: "/entity/stats/{model}/1",
				ExpectStatus: 200,
				Exclusions:   EntityStatsExclusions,
			},
			// 6. Get entity stats by state (global)
			{
				Name:         "Get entity statistics by state",
				Method:       "GET",
				PathTemplate: "/entity/stats/states",
				ExpectStatus: 200,
				Exclusions:   EntityStatsExclusions,
			},
			// 7. Get entity stats by state for model
			{
				Name:         "Get entity statistics by state for model",
				Method:       "GET",
				PathTemplate: "/entity/stats/states/{model}/1",
				ExpectStatus: 200,
				Exclusions:   EntityStatsExclusions,
			},
			// 8. GetAll entities
			{
				Name:         "GetAll entities",
				Method:       "GET",
				PathTemplate: "/entity/{model}/1",
				ExpectStatus: 200,
				Exclusions:   EntityEnvelopeExclusions,
			},
			// 9. Batch delete
			{
				Name:         "Batch delete entities",
				Method:       "DELETE",
				PathTemplate: "/entity/{model}/1",
				ExpectStatus: 200,
				Exclusions:   []string{"/0/entityModelClassId"},
			},
			// 10. GetAll after delete — expect empty
			{
				Name:         "GetAll after delete — expect empty",
				Method:       "GET",
				PathTemplate: "/entity/{model}/1",
				ExpectStatus: 200,
				Exclusions:   EntityEnvelopeExclusions,
			},
		},
	}
}
