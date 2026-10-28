package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
)

// toRequest maps the A2A request onto an agent.Request, merging message-level
// and request-level metadata (request-level wins).
func (e *CLIExecutor) toRequest(ctx context.Context, ec *a2asrv.ExecutorContext) (*agent.Request, error) {
	meta := mergeMetadata(ec)

	dir, err := e.workspace.Resolve(contextKey(ec))
	if err != nil {
		return nil, err
	}

	attach := filepath.Join(dir, ".a2a", "attachments", turnDir(ec))
	in, err := e.promptFromParts(ec.Message, attach)
	if err != nil {
		return nil, err
	}

	blocks, images, files := in.Blocks, in.Images, in.Files
	if len(blocks) == 0 && len(images) == 0 && len(files) == 0 && len(in.ImageFiles) == 0 {
		return nil, errNoText
	}

	sid, _ := e.sessions.Get(contextKey(ec))

	return &agent.Request{
		Prompt:       strings.Join(blocks, "\n"),
		Blocks:       blocks,
		SystemExtra:  metaString(meta, "harbor.systemPrompt"),
		Model:        metaString(meta, "harbor.model"),
		Agent:        metaString(meta, "harbor.agent"),
		WorkDir:      dir,
		SessionID:    sid,
		Continue:     sid == "" && metaBool(meta, "harbor.continue"),
		Fork:         metaBool(meta, "harbor.fork"),
		MaxTurns:     metaInt(meta, "harbor.maxTurns"),
		OutputSchema: metaMap(meta, "harbor.outputSchema"),
		Images:       images,
		Files:        files,
		ImageFiles:   in.ImageFiles,
		Meta:         meta,
	}, nil
}

// contextKey scopes a context id to its owner, so one caller cannot resume
// another caller's CLI session or working tree by reusing a contextId. A NUL
// separator keeps the user and context parts unambiguous.
func contextKey(ec *a2asrv.ExecutorContext) string {
	if ec.User == nil || ec.User.Name == "" {
		return ec.ContextID
	}
	return ec.User.Name + "\x00" + ec.ContextID
}

// turnDir is the per-turn subdirectory inbound attachments are materialized
// into, derived from the task id so turns do not overwrite each other in the
// shared workspace.
func turnDir(ec *a2asrv.ExecutorContext) string {
	id := filepath.Base(string(ec.TaskID))
	if id == "" || id == "." || id == ".." || id == string(filepath.Separator) {
		return "turn"
	}

	return id
}

func isWaitingForInput(ec *a2asrv.ExecutorContext) bool {
	return ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired
}

// answerWithoutRunning reports whether a request is answered from existing
// state instead of starting a CLI run: a repeated message returns the earlier
// task, and a message to a finished task returns it unchanged.
func (e *CLIExecutor) answerWithoutRunning(
	ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool,
) bool {
	if orig, ok := e.duplicateTask(ctx, ec); ok {
		e.replayDuplicate(ec, orig, yield)
		return true
	}

	if ec.StoredTask != nil && isTerminalState(ec.StoredTask.Status.State) {
		yield(ec.StoredTask, nil)
		return true
	}

	return false
}

// isTerminalState reports whether a task state is final (no further execution).
func isTerminalState(s a2a.TaskState) bool {
	switch s {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	}

	return false
}

func codeForRequestError(err error) string {
	switch {
	case errors.Is(err, errNoText), errors.Is(err, errUnsupportedContent):
		return codeUnsupportedContent
	default:
		return codeInternal
	}
}

func codeForStartError(err error) string {
	switch {
	case errors.Is(err, agent.ErrCLINotFound):
		return codeCLINotFound
	default:
		return codeInternal
	}
}

func mergeMetadata(ec *a2asrv.ExecutorContext) map[string]any {
	meta := map[string]any{}
	if ec.Message != nil {
		for k, v := range ec.Message.Metadata {
			meta[k] = v
		}
	}

	for k, v := range ec.Metadata {
		meta[k] = v
	}

	return meta
}
