package web

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestTaskComposerAccessibleControls(t *testing.T) {
	for _, voice := range []string{"", "/api/dictation/token"} {
		r := httptest.NewRecorder()
		w := workflowRow{ID: "task", Name: "Task", Definition: workflow.Starting}
		renderWorkflows(r, "list", map[string]any{"Workflows": []workflowRow{w}, "Tasks": []workflowRow{w}, "Dictation": voice})
		for _, want := range []string{`for="task">New task</label>`, `aria-describedby="task-hint"`, `id="task-error" role="alert"`, `id="submit-task" disabled`, `class="composer-toolbar task-actions"`, `id="task-attachments"`, `Cmd/Ctrl + Enter`} {
			if !strings.Contains(r.Body.String(), want) {
				t.Errorf("composer missing %s", want)
			}
		}
	}
}

// Exercise the actual task submission script, including an upload in flight and a failed API call.
func TestTaskComposerSubmission(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("workflows.html")
	if err != nil {
		t.Fatal(err)
	}
	script, _, ok := strings.Cut(string(src), "// keep the runs current")
	_, script, ok2 := strings.Cut(script, "// Task composer submission")
	if !ok || !ok2 {
		t.Fatal("no composer submission script")
	}
	out, err := exec.Command(node, "-e", composerHarness, "// Task composer submission"+script).CombinedOutput()
	if err != nil {
		t.Fatalf("composer: %v\n%s", err, out)
	}
}

const composerHarness = `
const assert = require('assert');
const el = () => ({ value: '', disabled: false, hidden: true, textContent: '', dataset: {},
  addEventListener(k, fn) { this[k] = fn; }, setAttribute(k,v) { this[k] = v; }, focus() {} });
const ids = Object.fromEntries(['task','submit-task','task-workflow','task-dictation','task-error','task-attachments','task-status'].map(k => [k, el()]));
global.document = { getElementById: k => ids[k] };
global.location = {};
let upload, locked = false, posts = 0, fail = true;
global.imageAttachments = () => ({ setDisabled(v) { locked = v; }, prompt: () => new Promise(r => upload = r) });
global.api = async (method, path, body) => { posts++; assert.equal(path, '/api/workflow-runs'); assert.equal(body.input, 'task\nimage'); if (fail) throw Error('Try again'); return {id:'run'}; };
eval(process.argv[1]);
const tick = () => new Promise(r => setTimeout(r, 0));
(async () => {
  const text = ids.task, button = ids['submit-task'];
  assert(button.disabled, 'blank task disabled');
  text.value = 'task'; text.input(); assert(!button.disabled);
  button.click({currentTarget:button});
  assert(button.disabled && text.readOnly && locked, 'lock while uploading');
  button.click({currentTarget:button}); assert.equal(posts,0);
  upload('task\nimage'); await tick();
  assert.equal(posts,1); assert(!button.disabled && !text.readOnly && !locked);
  assert.equal(text.value,'task'); assert.equal(ids['task-error'].textContent,'Try again');
  fail = false; button.click({currentTarget:button}); upload('task\nimage'); await tick();
  assert.equal(posts,2); assert.equal(location.href,'/workflow-runs/run');
  assert(button.disabled, 'stay locked until navigation');
})();
`
