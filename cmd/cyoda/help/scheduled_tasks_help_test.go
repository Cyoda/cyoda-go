package help

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestScheduledTasksTopic_CoversTheContract: the topic names every parameter,
// every DTO field, every status and failure reason, and every error code of
// GET /scheduled-tasks, read from the published contract so a later field
// cannot be added without documenting it.
func TestScheduledTasksTopic_CoversTheContract(t *testing.T) {
	topic := DefaultTree.Find([]string{"scheduled-tasks"})
	if topic == nil {
		t.Fatal("help topic scheduled-tasks is missing")
	}
	body := string(topic.Body)
	doc, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	op := doc.Paths.Find("/scheduled-tasks").Get
	var names []string
	for _, p := range op.Parameters {
		names = append(names, p.Value.Name)
	}
	for name := range doc.Components.Schemas["ScheduledTaskDto"].Value.Properties {
		names = append(names, name)
	}
	for _, s := range []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskRunning, spi.ScheduledTaskFailed} {
		names = append(names, string(s))
	}
	for _, r := range []spi.ScheduledTaskFailureReason{spi.FailureUnsafeWorkNotCompleted, spi.FailureOwnerLostRepeatedly,
		spi.FailureExpiredAfterFailedAttempts, spi.FailureRunPanicked, spi.FailureStoppedAfterPartialCommit} {
		names = append(names, string(r))
	}
	for _, name := range names {
		if !strings.Contains(body, "`"+name+"`") {
			t.Errorf("topic does not name `%s`", name)
		}
	}
	for _, code := range []string{"errors.BAD_REQUEST", "errors.UNAUTHORIZED", "errors.SERVER_ERROR", "errors.STORAGE_UNAVAILABLE"} {
		if !strings.Contains(body, code) {
			t.Errorf("topic ERRORS does not name %s", code)
		}
	}
	if !strings.Contains(body, "GET  /api/scheduled-tasks") {
		t.Errorf("topic SYNOPSIS does not show the endpoint")
	}
}

var openapiPathCount = regexp.MustCompile(`The spec declares (\d+) paths`)

// TestOpenAPITopic_PathCountMatchesSpec keeps the openapi topic's path count
// equal to the embedded spec's.
func TestOpenAPITopic_PathCountMatchesSpec(t *testing.T) {
	topic := DefaultTree.Find([]string{"openapi"})
	if topic == nil {
		t.Fatal("help topic openapi is missing")
	}
	m := openapiPathCount.FindStringSubmatch(string(topic.Body))
	if m == nil {
		t.Fatal(`openapi topic has no "The spec declares N paths" sentence`)
	}
	doc, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	if got, _ := strconv.Atoi(m[1]); got != doc.Paths.Len() {
		t.Errorf("openapi topic says %d paths; the spec declares %d", got, doc.Paths.Len())
	}
}

// TestSchedulerConfigTopic_SeesScheduledTasks: config.scheduler points the
// reader at the list of the tasks the scheduler runs.
func TestSchedulerConfigTopic_SeesScheduledTasks(t *testing.T) {
	topic := DefaultTree.Find([]string{"config", "scheduler"})
	if topic == nil {
		t.Fatal("help topic config.scheduler is missing")
	}
	found := false
	for _, s := range topic.SeeAlso {
		if s == "scheduled-tasks" {
			found = true
		}
	}
	if !found {
		t.Errorf("config.scheduler see_also = %v, want it to list scheduled-tasks", topic.SeeAlso)
	}
	if !strings.Contains(string(topic.Body), "- scheduled-tasks") {
		t.Error("config.scheduler SEE ALSO does not list scheduled-tasks")
	}
}
