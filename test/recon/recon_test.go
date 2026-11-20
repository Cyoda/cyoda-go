//go:build cyoda_recon

package recon

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cyoda/cyoda-go/app"
)

func TestReconcile(t *testing.T) {
	cfg := loadCloudConfig()
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		t.Skip("CYODA_CLOUD_CLIENT_ID and CYODA_CLOUD_CLIENT_SECRET must be set")
	}

	// Start Cyoda-Go in-process.
	miniCfg := app.DefaultConfig()
	miniCfg.ContextPath = ""
	miniApp := app.New(miniCfg)
	miniSrv := httptest.NewServer(miniApp.Handler())
	defer miniSrv.Close()

	// Create OAuth2 client for Cyoda Cloud.
	cloudClient := newCyodaCloudClient(cfg)

	// Run all scenarios.
	scenarios := modelScenarios()
	scenarios = append(scenarios, entityScenarios()...)
	scenarios = append(scenarios, workflowScenarios()...)
	scenarios = append(scenarios, messagingScenarios()...)
	var scenarioReports []ScenarioReport

	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			report := reconcile(scenario, miniSrv.URL, cfg.BaseURL, cloudClient)
			scenarioReports = append(scenarioReports, report)

			for _, step := range report.Steps {
				if !step.StatusMatch {
					t.Logf("STATUS MISMATCH in %q: Mini=%d Cloud=%d",
						step.StepName, step.MiniStatus, step.CloudStatus)
				}
				if !step.BodyMatch {
					t.Logf("BODY MISMATCH in %q", step.StepName)
				}
			}
		})
	}

	// Generate and write report.
	run := RunReport{
		Date:       time.Now(),
		CyodaGo:    "in-process",
		CyodaCloud: cfg.BaseURL,
		Scenarios:  scenarioReports,
	}
	md := generateReport(run)

	// Write report to test/recon/.results/ (gitignored).
	// Resolve absolute path so the output is always findable regardless of CWD.
	resultsDir, _ := filepath.Abs(".results")
	os.MkdirAll(resultsDir, 0755)
	filename := fmt.Sprintf("recon_report_%s.md", time.Now().Format("2006-01-02T150405"))
	outPath := filepath.Join(resultsDir, filename)
	if err := os.WriteFile(outPath, []byte(md), 0644); err != nil {
		t.Fatalf("failed to write report: %v", err)
	}
	t.Logf("Report written to %s", outPath)
}
