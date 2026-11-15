package web

import (
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestResumeButtonWorksAfterRunsTableRefresh(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	_, script, ok := strings.Cut(workflowsHTML, "// Resume controls")
	if !ok {
		t.Fatal("no Resume controls")
	}
	script, _, ok = strings.Cut(script, `document.querySelectorAll("[data-default-workflow]")`)
	if !ok {
		t.Fatal("no end of Resume controls")
	}
	out, err := exec.Command(node, "-e", resumeControlsHarness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("Resume after refresh: %v\n%s", err, out)
	}
}

const resumeControlsHarness = `
const assert = require('assert'), vm = require('vm');
const listeners = {}, calls = []; let reloaded = 0;
const document = { querySelectorAll: () => [], addEventListener: (kind, fn) => listeners[kind] = fn };
vm.runInNewContext(process.argv[1], {document, api: async (...args) => calls.push(args),
 location: {reload: () => reloaded++}, err: (message) => {throw Error(message)} });
(async () => {
 // The poll replaces the table, so this button did not exist when handlers were installed.
 const button = {dataset: {resumeRun: 'run-1'}, disabled: false};
 const event = {target: {closest: selector => selector === '[data-resume-run]' ? button : null}};
 if (listeners.click) await listeners.click(event);
 assert.equal(calls.length, 1, 'new Resume button must send a request');
 assert.equal(calls[0][0], 'POST'); assert.equal(calls[0][1], '/api/workflow-runs/run-1/resume');
 assert.equal(JSON.stringify(calls[0][2]), '{}'); assert.equal(reloaded, 1);
 await listeners.click(event); assert.equal(calls.length, 1, 'disabled button must not send twice');
})().catch(e => {console.error(e); process.exit(1)});
`

// Execute the actual run template script, rather than an editor or list fragment.
func TestRunPageResumeIsWiredAfterRefresh(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	rec := httptest.NewRecorder()
	if err := workflowsTmpl.ExecuteTemplate(rec, "run", map[string]any{"Version": "test", "Run": runRow{ID: "run-1", Title: "Sample", Status: workflow.Stalled, Steps: []workflow.Step{{Title: "Build"}}}, "Now": t0, "Agents": []agentRow{}, "Steps": []stepView{}, "Events": []runEvent{}}); err != nil {
		t.Fatal(err)
	}
	page := rec.Body.String()
	var script string
	for rest := page; ; {
		_, body, ok := strings.Cut(rest, "<script>")
		if !ok {
			break
		}
		candidate, tail, ok := strings.Cut(body, "</script>")
		if !ok {
			t.Fatal("unterminated script")
		}
		if strings.Contains(candidate, "const run =") {
			script = candidate
			break
		}
		rest = tail
	}
	if script == "" {
		t.Fatal("missing run script")
	}
	out, err := exec.Command(node, "-e", runResumeHarness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("run Resume: %v\n%s", err, out)
	}
}

const runResumeHarness = `
const assert=require('assert'),vm=require('vm');
const first={disabled:false},replacement={disabled:false},calls=[];
let current=first;
const live={innerHTML:'old',replaceWith(){current=replacement}};
const document={querySelector:s=>s==='#resume'?current:null,
 getElementById:id=>id==='live'?live:null};
class DOMParser {parseFromString(){return {title:'Sample',getElementById:id=>id==='live'?{innerHTML:'new'}:null}}}
const fetch=async(path,options)=>{calls.push({path,options});return {ok:true,text:async()=>''}};
vm.runInNewContext(process.argv[1],{document,fetch,DOMParser,window:{},location:{href:'/workflow-runs/run-1'},setInterval:()=>{}});
(async()=>{
 assert.equal(typeof first.onclick,'function','run page Resume must be wired');
 const pending=first.onclick({currentTarget:first,target:first});
 assert.equal(first.disabled,true,'disable while resuming');
 await pending;
 const posts=calls.filter(c=>c.options?.method==='POST');
 assert.equal(posts.length,1);assert.equal(posts[0].path,'/api/workflow-runs/run-1/resume');
 assert.equal(posts[0].options.headers['Content-Type'],'application/json');assert.equal(posts[0].options.body,'{}');
 assert.equal(typeof replacement.onclick,'function','refreshed run page Resume must be wired');
})().catch(e=>{console.error(e);process.exit(1)});
`
