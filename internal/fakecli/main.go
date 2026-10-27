// Command fakecli is the test helper standing in for real coding-agent CLIs.
// It dispatches on the base name of its own executable, so the test helper
// builds one binary and copies it under each fake name (fake-usage, fake-acp,
// ...). Being a native binary it runs identically on every platform.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	switch name {
	case "fake-acp":
		acpMain()
	case "fake-codebuddy":
		codebuddyMain()
	case "fake-codex":
		codexMain()
	case "fake-docker":
		dockerMain()
	case "fake-fail":
		fmt.Fprintln(os.Stderr, "boom: something went wrong")
		os.Exit(3)
	case "fake-form":
		formMain()
	case "fake-home":
		fmt.Printf("HOME=%s\n", os.Getenv("HOME"))
	case "fake-stdin-echo":
		stdinEchoMain()
	case "fake-arg-log":
		argLogMain()
	case "fake-noop":
		// nothing to do
	case "fake-mutate":
		mutateMain()
	case "fake-mycoder":
		out("noise line")
		out("OUT: hello from my-coder")
	case "fake-opencode":
		out("listing files")
		out("a.go b.go")
	case "fake-permission":
		permissionMain()
	case "fake-plan":
		planMain()
	case "fake-pwd":
		wd, _ := os.Getwd()
		out(wd)
	case "fake-question":
		questionMain()
	case "fake-question-multi":
		questionMultiMain()
	case "fake-resume":
		resumeMain()
	case "fake-server-stderr":
		fmt.Fprintln(os.Stderr, "endpoint: http://127.0.0.1:12345/api/v1/acp")
		time.Sleep(30 * time.Second)
	case "fake-slow":
		slowMain()
	case "fake-structured":
		structuredMain()
	case "fake-trace":
		traceMain()
	case "fake-usage":
		usageMain()
	case "fake-pwd-env":
		// Reports the PWD environment variable, for tests asserting the
		// launcher's PWD handling without a shell re-normalizing the value.
		out(os.Getenv("PWD"))
	case "codex":
		// Named "codex" (as the real CLI), this mode serves the auth probe and
		// falls back to the exec JSONL emitter.
		if len(os.Args) > 1 && os.Args[1] == "login" {
			if len(os.Args) > 2 && os.Args[2] == "status" {
				out("Not logged in")
				os.Exit(1)
			}
			out("ok")
			return
		}
		codexMain()
	case "fake-codex-appserver":
		codexAppServerMain()
	case "codebuddy":
		// Named "codebuddy" (as the real CLI), prints the version from
		// FAKE_VERSION for profile/min-version probing tests.
		out(os.Getenv("FAKE_VERSION"))
	default:
		fmt.Fprintf(os.Stderr, "fakecli: unknown mode %q\n", name)
		os.Exit(2)
	}
}

func out(s string) { fmt.Println(s) }

// emit writes one NDJSON frame to stdout.
func emit(frame string) { fmt.Println(frame) }

// readLine reads one stdin line; on EOF it returns an empty string, matching
// the shell scripts that proceed after a failed read.
func readLine() string {
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

// appendLog appends line to the file named by env, when set.
func appendLog(env, line string) {
	path := os.Getenv(env)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// fake-acp implements initialize, session/new and one prompt turn that
// requests permission, streams a thought and two message chunks, and ends the
// turn with a stop reason and usage.
func acpMain() {
	idOf := func(line string) string {
		var f struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(line), &f) != nil {
			return "null"
		}
		return string(f.ID)
	}

	line := readLine()
	emit(`{"jsonrpc":"2.0","id":` + idOf(line) + `,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[],"agentInfo":{"name":"fake","version":"0"}}}`)

	line = readLine()
	appendLog("FAKE_ACP_LOG", line)
	emit(`{"jsonrpc":"2.0","id":` + idOf(line) + `,"result":{"sessionId":"ses_fake"}}`)

	line = readLine()
	promptID := idOf(line)
	emit(`{"jsonrpc":"2.0","id":9001,"method":"session/request_permission","params":{"sessionId":"ses_fake","toolCall":{"toolCallId":"tc1","title":"run tests","kind":"execute"},"options":[{"optionId":"allow_once","name":"Allow","kind":"allow_once"},{"optionId":"reject_once","name":"Reject","kind":"reject_once"}]}}`)
	readLine() // the client's answer

	emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}}`)
	emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello"}}}}`)
	emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" world"}}}}`)
	emit(`{"jsonrpc":"2.0","id":` + promptID + `,"result":{"stopReason":"end_turn","usage":{"inputTokens":7,"outputTokens":2,"totalTokens":9}}}`)

	time.Sleep(10 * time.Second)
}

// fake-codebuddy emits claude/codebuddy-style stream-json NDJSON.
func codebuddyMain() {
	appendLog("HARBOR_ARG_LOG", strings.Join(os.Args[1:], " "))
	emit(`{"type":"system","subtype":"init","session_id":"sess-123"}`)
	time.Sleep(50 * time.Millisecond)
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello "}]},"session_id":"sess-123"}`)
	time.Sleep(50 * time.Millisecond)
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]},"session_id":"sess-123"}`)
	emit(`{"type":"result","subtype":"success","result":"Hello world","session_id":"sess-123","is_error":false}`)
}

// fake-codex emits codex exec --json style JSONL events.
func codexMain() {
	appendLog("HARBOR_ARG_LOG", strings.Join(os.Args[1:], " "))
	emit(`{"type":"thread.started","thread_id":"t-1"}`)
	emit(`{"type":"turn.started"}`)
	emit(`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Hello from codex"}}`)
	emit(`{"type":"turn.completed"}`)
}

// fake-docker records the argv, then execs the command that follows the image
// token so the wrapped CLI actually runs.
func dockerMain() {
	appendLog("FAKE_DOCKER_LOG", strings.Join(os.Args[1:], " "))
	image := os.Getenv("FAKE_DOCKER_IMAGE")
	rest := []string{}
	found := false
	for _, a := range os.Args[1:] {
		if found {
			rest = append(rest, a)
			continue
		}
		if a == image {
			found = true
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "fake-docker: image %s not found\n", image)
		os.Exit(1)
	}

	cmd := exec.Command(rest[0], rest[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		os.Exit(1)
	}
}

// fake-form asks a form question via an elicitation_create control request,
// logs the control_response, then emits the terminal result.
func formMain() {
	readLine()
	emit(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserForStructuredInput","input":{"message":"Pick a color"}}]}}`)
	emit(`{"type":"control_request","request_id":"el_1","request":{"subtype":"elicitation_create","mode":"form","message":"Pick a color","requestedSchema":{"type":"object","properties":{"color":{"type":"string","enum":["red","blue"]}},"required":["color"]}}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		appendLog("HARBOR_STDIN_LOG", line)
		if strings.Contains(line, "control_response") {
			break
		}
	}
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"chose blue"}]}}`)
	emit(`{"type":"result","result":"chose blue","is_error":false}`)
}

// fake-mutate modifies a tracked file in its working directory.
func mutateMain() {
	f, err := os.OpenFile("tracked.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "append: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(f, "appended line")
	f.Close()
	out("done")
}

// fake-permission asks for tool permission via a control_request, waits for
// the control_response, then emits the terminal result.
func permissionMain() {
	readLine()
	emit(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}`)
	emit(`{"type":"control_request","request_id":"perm_test_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo hi"},"tool_use_id":"toolu_1"}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "control_response") {
			break
		}
	}
	emit(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi","is_error":false}]}}`)
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"allowed"}]}}`)
	emit(`{"type":"result","result":"allowed","is_error":false}`)
}

// fake-plan requests plan approval via ExitPlanMode (can_use_tool), logs the
// control_response, then emits the terminal result.
func planMain() {
	emit(`{"type":"control_request","request_id":"plan_1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"Create README.md"}}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "control_response") {
			appendLog("HARBOR_STDIN_LOG", line)
			break
		}
	}
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"approved"}]}}`)
	emit(`{"type":"result","result":"approved","is_error":false}`)
}

// fake-question asks a question via an AskUserQuestion can_use_tool control
// request, waits for the answer, then emits the terminal result.
func questionMain() {
	readLine()
	emit(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"Favorite color?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":""},{"label":"Blue","description":""}]}]}}]}}`)
	emit(`{"type":"control_request","request_id":"perm_q1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Favorite color?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":""},{"label":"Blue","description":""}]}]},"tool_use_id":"t1"}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "control_response") {
			break
		}
	}
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"chose Blue"}]}}`)
	emit(`{"type":"result","result":"chose Blue","is_error":false}`)
}

// fake-question-multi asks two questions in one AskUserQuestion can_use_tool
// request and records the control_response the client sends back.
func questionMultiMain() {
	readLine()
	emit(`{"type":"control_request","request_id":"perm_qm","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Favorite color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]},{"question":"Favorite animal?","header":"Animal","options":[{"label":"Cat"},{"label":"Dog"}]}]},"tool_use_id":"t1"}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "control_response") {
			appendLog("FAKE_QA_LOG", line)
			break
		}
	}
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"answered"}]}}`)
	emit(`{"type":"result","result":"answered","is_error":false}`)
}

// fake-resume supports resume recovery: the first launch emits a session id
// and a permission request then exits (the task pauses on input-required); a
// launch with --resume echoes a terminal result so the recovered task
// completes.
func resumeMain() {
	if strings.Contains(strings.Join(os.Args[1:], " "), "--resume") {
		readLine() // injected answer prompt
		emit(`{"type":"result","result":"resumed","is_error":false}`)
		return
	}
	readLine()
	emit(`{"type":"system","subtype":"init","session_id":"sess-xyz"}`)
	emit(`{"type":"control_request","request_id":"perm_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo hi"}}}`)
}

// fake-slow stays alive between init and result for cancellation tests.
func slowMain() {
	emit(`{"type":"system","subtype":"init","session_id":"slow-1"}`)
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"slow-1"}`)
	time.Sleep(30 * time.Second)
	emit(`{"type":"result","subtype":"success","result":"done","session_id":"slow-1","is_error":false}`)
}

// fake-structured emits a schema-validated structured result.
func structuredMain() {
	readLine()
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"Paris, 2.1M"}]}}`)
	emit(`{"type":"result","result":"Paris, 2.1M","is_error":false,"structured_output":{"city":"Paris","population_millions":2.1}}`)
}

// fake-trace emits reasoning, a tool call/result, and usage.
func traceMain() {
	emit(`{"type":"system","subtype":"init","session_id":"sess-t"}`)
	emit(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"let me think"}]},"session_id":"sess-t"}`)
	emit(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},"session_id":"sess-t"}`)
	emit(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"session_id":"sess-t"}`)
	emit(`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"session_id":"sess-t"}`)
	emit(`{"type":"result","subtype":"success","result":"done","session_id":"sess-t","is_error":false,"total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":3}}`)
}

// fake-usage emits a terminal result carrying token/cost usage.
func usageMain() {
	readLine()
	emit(`{"type":"result","result":"ok","is_error":false,"total_cost_usd":0.0123,"usage":{"input_tokens":11,"output_tokens":22}}`)
}

// fake-codex-appserver speaks just enough of the codex app-server protocol:
// it answers initialize, thread/start and turn/start, then streams a turn's
// notifications. With FAKE_ITEMS=1 the turn carries item/completed
// notifications (reasoning, command_execution, file_change) instead of
// message deltas.
func codexAppServerMain() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	respond := func(line, result string) {
		var f struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal([]byte(line), &f)
		emit(`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":` + result + `}`)
	}

	for sc.Scan() {
		line := sc.Text()
		var f struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &f) != nil {
			continue
		}
		switch f.Method {
		case "initialize":
			respond(line, `{"userAgent":"fake"}`)
		case "thread/start":
			respond(line, `{"thread":{"id":"th_1"}}`)
		case "turn/start":
			respond(line, `{"turn":{"id":"tu_1"}}`)
			if os.Getenv("FAKE_ITEMS") == "1" {
				emit(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"th_1","item":{"type":"reasoning","text":"thinking"}}}`)
				emit(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"th_1","item":{"type":"command_execution","command":"ls"}}}`)
				emit(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"th_1","item":{"type":"file_change","changes":{"/w/a.go":{"kind":"update"}}}}}`)
			} else {
				emit(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"th_1","delta":"po"}}`)
				emit(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"th_1","delta":"ng"}}`)
			}
			emit(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}}`)
		}
	}

	time.Sleep(30 * time.Second)
}

// fake-stdin-echo echoes the first stdin line to HARBOR_STDIN_LOG, then emits
// a terminal result.
func stdinEchoMain() {
	line := readLine()
	appendLog("HARBOR_STDIN_LOG", line)
	emit(`{"type":"result","result":"ok","is_error":false}`)
}

// fake-arg-log appends the argv to HARBOR_ARG_LOG and prints FAKE_OUTPUT, for
// tests that assert on the command line the launcher built.
func argLogMain() {
	appendLog("HARBOR_ARG_LOG", strings.Join(os.Args[1:], " "))
	if v := os.Getenv("FAKE_OUTPUT"); v != "" {
		fmt.Print(v)
	}
}
