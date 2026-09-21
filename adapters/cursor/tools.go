package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/tools"
)

const defaultToolTimeout = 30 * time.Second
const maximumToolPreviewBytes = 4096
const maximumApprovalPromptRunes = 1500
const maximumApprovalPromptBytes = 6000

var actionHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var secretLikePattern = regexp.MustCompile(`(?i)(-----BEGIN [A-Z ]*PRIVATE KEY-----|\bauthorization\s*:\s*(?:bearer|basic)\s+["']?[A-Za-z0-9_./+=-]{8,}|\b(?:[A-Z0-9]+[_-])*(?:API[_-]?KEY|ACCESS[_-]?TOKEN|REFRESH[_-]?TOKEN|SESSION[_-]?TOKEN|TOKEN|PASSWORD|PASSWD|CLIENT[_-]?SECRET|SECRET(?:[_-]?ACCESS[_-]?KEY)?|PRIVATE[_-]?KEY)\s*[:=]\s*["']?[A-Za-z0-9_./+=-]{8,}|\b(?:gh[opsu]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,})|\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]{2,}|\bhttps?://[^\s/:@]+:[^\s/@]+@)`)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

type workerToolRequest struct {
	AttemptKey string          `json:"attemptKey"`
	CallID     string          `json:"callId"`
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
}

type workerCommandArgs struct {
	Command         string `json:"command"`
	CWD             string `json:"cwd"`
	WorkspaceAccess string `json:"workspaceAccess"`
	TimeoutMS       *int64 `json:"timeoutMs"`
}

type workerFileChangeArgs struct {
	Changes []workerFileChange `json:"changes"`
}

type workerFileChange struct {
	Path           string  `json:"path"`
	Operation      string  `json:"operation"`
	ExpectedSHA256 *string `json:"expectedSha256"`
	Content        *string `json:"content"`
}

type workerToolResult struct {
	Success           bool                     `json:"success"`
	Output            string                   `json:"output,omitempty"`
	OutputUnavailable bool                     `json:"outputUnavailable,omitempty"`
	Truncated         bool                     `json:"truncated,omitempty"`
	ExitCode          *int                     `json:"exitCode,omitempty"`
	Changes           []workerFileChangeResult `json:"changes,omitempty"`
}

type workerFileChangeResult struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

type pendingApproval struct {
	attempt    *attemptRuntime
	actionHash string
	decision   chan string
	resolved   bool
}

func (adapter *Adapter) handleWorkerRequest(frame bridgeFrame) {
	result, code, runtime := adapter.executeWorkerTool(frame)
	if err := adapter.bridge.respond(frame.ID, result, code); err != nil && runtime != nil {
		runtime.failUnknown()
	}
}

func (adapter *Adapter) executeWorkerTool(frame bridgeFrame) (workerToolResult, string, *attemptRuntime) {
	var payload workerToolRequest
	if decodeStrict(frame.Payload, &payload) != nil || !boundedNativeID(payload.CallID) || payload.Name == "" || len(payload.Name) > 200 || !utf8.ValidString(payload.Name) {
		return workerToolResult{}, "tool_request_invalid", nil
	}
	adapter.mu.Lock()
	runtime := adapter.attempts[payload.AttemptKey]
	adapter.mu.Unlock()
	if runtime == nil {
		return workerToolResult{}, "tool_attempt_stale", nil
	}
	toolName, ok := cursorToolLabel(payload.Name)
	if !ok {
		return workerToolResult{}, "tool_request_invalid", runtime
	}
	request, err := runnerRequest(runtime.workspace, payload)
	if err != nil || runtime.approvalMode != harnessadapter.ApprovalModeExplicitOnce || adapter.runner == nil {
		return workerToolResult{}, "tool_request_invalid", runtime
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return workerToolResult{}, "tool_request_invalid", runtime
	}
	actionHash := digestString("cursor-tool-action-v1\x00" + attemptKey(runtime.reference) + "\x00" + runtime.policyHash + "\x00" + toolName + "\x00" + string(encoded))
	callID := derivedUUID("cursor-tool", attemptKey(runtime.reference)+"\x00"+payload.CallID)
	input, safePrompt := safeToolInput(request)

	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return workerToolResult{}, "tool_attempt_stale", runtime
	}
	if _, duplicate := runtime.tools[payload.CallID]; duplicate {
		runtime.mu.Unlock()
		runtime.failUnknown()
		return workerToolResult{}, "tool_call_duplicate", runtime
	}
	runtime.tools[payload.CallID] = toolState{callID: callID, toolName: toolName, actionHash: actionHash, started: true}
	runtime.mu.Unlock()
	runtime.push(harnessadapter.ToolStartedEvent{
		EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, CallID: callID,
		ToolName: toolName, ActionHash: actionHash, Input: input,
	})

	effectful := request.Kind == toolrunner.KindFileChange || request.Command.Access == toolrunner.AccessWrite
	if effectful {
		decision, ok := adapter.awaitApproval(runtime, payload.CallID, callID, actionHash, safePrompt)
		if !ok {
			adapter.completeTool(runtime, payload.CallID, callID, actionHash, "failed", "none", unavailableToolResult(), nil, false)
			return workerToolResult{}, "tool_cancelled", runtime
		}
		if decision == "deny" {
			adapter.completeTool(runtime, payload.CallID, callID, actionHash, "failed", "none", unavailableToolResult(), nil, false)
			return workerToolResult{}, "tool_denied", runtime
		}
	}

	if runtime.context.Err() != nil {
		adapter.completeTool(runtime, payload.CallID, callID, actionHash, "failed", "none", unavailableToolResult(), nil, false)
		return workerToolResult{}, "tool_cancelled", runtime
	}
	result, err := adapter.runner.Run(runtime.context, request)
	if err != nil {
		effectStatus := "none"
		if effectful {
			effectStatus = "unknown"
		}
		adapter.completeTool(runtime, payload.CallID, callID, actionHash, "unknown", effectStatus, harnessprotocol.SafeContent{Kind: "unavailable", Reason: "not_observed", Redaction: "unknown"}, nil, false)
		if effectful {
			runtime.failUnknown()
		}
		return workerToolResult{}, "tool_execution_unknown", runtime
	}
	workerResult, content, fullText, incomplete := safeToolResult(result)
	status := "failed"
	if result.Success {
		status = "succeeded"
	}
	effectStatus := "none"
	if effectful {
		effectStatus = "known"
	}
	if len(result.Output) > 0 || result.Truncated {
		runtime.push(harnessadapter.ToolOutputEvent{
			EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, CallID: callID,
			ChunkIndex: 0, Stream: "result", Output: content, FullText: fullText, FullTextIncomplete: incomplete,
		})
	}
	adapter.completeTool(runtime, payload.CallID, callID, actionHash, status, effectStatus, content, fullText, incomplete)
	return workerResult, "", runtime
}

func (adapter *Adapter) awaitApproval(runtime *attemptRuntime, nativeCallID, callID, actionHash, safePrompt string) (string, bool) {
	approvalID := derivedUUID("cursor-approval", attemptKey(runtime.reference)+"\x00"+nativeCallID)
	pending := &pendingApproval{attempt: runtime, actionHash: actionHash, decision: make(chan string, 1)}
	adapter.mu.Lock()
	_, duplicate := adapter.approvals[approvalID]
	if !duplicate {
		adapter.approvals[approvalID] = pending
	}
	adapter.mu.Unlock()
	if duplicate {
		runtime.failUnknown()
		return "", false
	}
	runtime.mu.Lock()
	state := runtime.tools[nativeCallID]
	state.approvalID = approvalID
	runtime.tools[nativeCallID] = state
	runtime.mu.Unlock()
	runtime.setWaitingApproval(true)
	runtime.push(harnessadapter.ApprovalRequestedEvent{
		EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, ApprovalID: approvalID,
		CallID: callID, ActionHash: actionHash, SafePrompt: safePrompt,
	})
	defer func() {
		runtime.setWaitingApproval(false)
		adapter.mu.Lock()
		delete(adapter.approvals, approvalID)
		adapter.mu.Unlock()
	}()
	select {
	case decision := <-pending.decision:
		return decision, true
	case <-runtime.context.Done():
		adapter.mu.Lock()
		pending.resolved = true
		adapter.mu.Unlock()
		return "", false
	}
}

func (adapter *Adapter) RespondApproval(_ context.Context, input harnessadapter.RespondApprovalInput) (harnessadapter.ResponseResult, error) {
	if !validReference(input.Attempt) || !uuidPattern.MatchString(input.ApprovalID) || input.ApprovalVersion != 2 ||
		!actionHashPattern.MatchString(input.ActionHash) || (input.Decision != "allow_once" && input.Decision != "deny") {
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected, Failure: protocolFailure("cursor_approval_invalid", "cursor approval response is invalid")}, nil
	}
	adapter.mu.Lock()
	pending, ok := adapter.approvals[input.ApprovalID]
	if !ok || pending.attempt.reference != input.Attempt || pending.actionHash != input.ActionHash || pending.resolved {
		adapter.mu.Unlock()
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected, Failure: taskFailure("cursor_approval_stale", "cursor approval request is no longer pending")}, nil
	}
	pending.resolved = true
	pending.decision <- input.Decision
	adapter.mu.Unlock()
	return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseApplied}, nil
}

func (adapter *Adapter) completeTool(runtime *attemptRuntime, nativeCallID, callID, actionHash, status, effectStatus string, content harnessprotocol.SafeContent, fullText *string, incomplete bool) {
	runtime.mu.Lock()
	state := runtime.tools[nativeCallID]
	if state.done || state.callID != callID || state.actionHash != actionHash {
		runtime.mu.Unlock()
		runtime.failUnknown()
		return
	}
	state.done = true
	runtime.tools[nativeCallID] = state
	runtime.mu.Unlock()
	effectRef := ""
	if effectStatus != "none" {
		effectRef = actionHash
	}
	runtime.push(harnessadapter.ToolCompletedEvent{
		EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, CallID: callID,
		Status: status, Result: content, EffectStatus: effectStatus, EffectRef: effectRef,
		FullText: fullText, FullTextIncomplete: incomplete,
	})
}

func runnerRequest(workspace string, payload workerToolRequest) (toolrunner.Request, error) {
	request := toolrunner.Request{CallID: payload.CallID, Workspace: workspace}
	switch payload.Name {
	case "cursor_command":
		var args workerCommandArgs
		if decodeStrict(payload.Args, &args) != nil {
			return toolrunner.Request{}, errors.New("invalid command arguments")
		}
		timeout := defaultToolTimeout
		if args.TimeoutMS != nil {
			if *args.TimeoutMS < 1 || *args.TimeoutMS > int64((60*time.Second)/time.Millisecond) {
				return toolrunner.Request{}, errors.New("invalid command timeout")
			}
			timeout = time.Duration(*args.TimeoutMS) * time.Millisecond
		}
		request.Kind = toolrunner.KindCommand
		request.Command = &toolrunner.CommandRequest{Command: args.Command, CWD: args.CWD, Access: toolrunner.Access(args.WorkspaceAccess), Timeout: timeout}
	case "cursor_file_change":
		var args workerFileChangeArgs
		if decodeStrict(payload.Args, &args) != nil {
			return toolrunner.Request{}, errors.New("invalid file change arguments")
		}
		changes := make([]toolrunner.FileChange, 0, len(args.Changes))
		for _, change := range args.Changes {
			if change.Operation == string(toolrunner.FileWrite) && change.Content == nil || change.Operation == string(toolrunner.FileDelete) && change.Content != nil {
				return toolrunner.Request{}, errors.New("invalid file change arguments")
			}
			content := []byte(nil)
			if change.Content != nil {
				content = []byte(*change.Content)
			}
			changes = append(changes, toolrunner.FileChange{Path: change.Path, Operation: toolrunner.FileOperation(change.Operation), ExpectedSHA256: change.ExpectedSHA256, Content: content})
		}
		request.Kind = toolrunner.KindFileChange
		request.FileChange = &toolrunner.FileChangeRequest{Changes: changes}
	default:
		return toolrunner.Request{}, errors.New("unknown cursor tool")
	}
	if err := toolrunner.ValidateRequest(request); err != nil {
		return toolrunner.Request{}, err
	}
	return request, nil
}

func safeToolResult(result toolrunner.Result) (workerToolResult, harnessprotocol.SafeContent, *string, bool) {
	validOutput := utf8.Valid(result.Output)
	fullOutput := ""
	output := ""
	outputTruncated := false
	if validOutput && !secretLikePattern.Match(result.Output) {
		fullOutput = sanitizeVisibleText(string(result.Output))
		output, outputTruncated = truncateUTF8(fullOutput, harnessprotocol.MaximumMessageBytes)
	}
	truncated := result.Truncated || outputTruncated
	workerResult := workerToolResult{Success: result.Success, Truncated: truncated, ExitCode: result.ExitCode}
	content := harnessprotocol.SafeContent{Kind: "inline", Redaction: "none", Truncated: truncated}
	var fullText *string
	if validOutput && !secretLikePattern.Match(result.Output) {
		workerResult.Output = output
		content.Content = workerResult.Output
		fullText = &fullOutput
	} else {
		workerResult.OutputUnavailable = true
		content = harnessprotocol.SafeContent{Kind: "unavailable", Reason: "provider_redacted", Redaction: "applied", Truncated: truncated}
	}
	if len(result.Changes) > 0 {
		workerResult.Changes = make([]workerFileChangeResult, 0, len(result.Changes))
		for _, change := range result.Changes {
			workerResult.Changes = append(workerResult.Changes, workerFileChangeResult{Path: change.Path, Operation: string(change.Operation)})
		}
	}
	return workerResult, content, fullText, result.Truncated
}

func cursorToolLabel(name string) (string, bool) {
	// The worker uses SDK-safe bare keys. Harness events and the canonical
	// policy manifest use these provider-neutral logical names.
	switch name {
	case "cursor_command":
		return "cursor.command", true
	case "cursor_file_change":
		return "cursor.file_change", true
	default:
		return "", false
	}
}

func safeToolInput(request toolrunner.Request) (harnessprotocol.SafeContent, string) {
	redacted := func(action string) (harnessprotocol.SafeContent, string) {
		return harnessprotocol.SafeContent{Kind: "unavailable", Reason: "provider_redacted", Redaction: "applied"},
			"Агент хочет " + action + ", но описание скрыто: оно похоже на секрет. Разрешить один раз?"
	}
	var preview, promptPrefix string
	if request.Kind == toolrunner.KindCommand {
		access := "только чтение"
		if request.Command.Access == toolrunner.AccessWrite {
			access = "изменение файлов"
		}
		preview = "Команда: " + sanitizeVisibleText(request.Command.Command) + "\nПапка: " + sanitizeVisibleText(request.Command.CWD) + "\nДоступ: " + access
		promptPrefix = "Агент хочет выполнить команду с записью в рабочей папке диалога."
		if secretLikePattern.MatchString(request.Command.Command) || secretLikePattern.MatchString(request.Command.CWD) {
			return redacted("выполнить команду с записью в рабочей папке диалога")
		}
	} else {
		lines := make([]string, 0, len(request.FileChange.Changes))
		for _, change := range request.FileChange.Changes {
			if secretLikePattern.MatchString(change.Path) {
				return redacted("изменить файлы в рабочей папке диалога")
			}
			operation := "Запись"
			if change.Operation == toolrunner.FileDelete {
				operation = "Удаление"
			}
			lines = append(lines, operation+": "+sanitizeVisibleText(change.Path))
		}
		preview = strings.Join(lines, "\n")
		promptPrefix = "Агент хочет изменить файлы в рабочей папке диалога."
	}
	preview, truncated := truncateUTF8(preview, maximumToolPreviewBytes)
	content := harnessprotocol.SafeContent{Kind: "inline", Content: preview, Redaction: "none", Truncated: truncated}
	return content, boundedApprovalPrompt(promptPrefix, preview, truncated)
}

func unavailableToolResult() harnessprotocol.SafeContent {
	return harnessprotocol.SafeContent{Kind: "unavailable", Reason: "not_observed", Redaction: "unknown"}
}

func truncateUTF8(value string, maximum int) (string, bool) {
	if len(value) <= maximum {
		return value, false
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func boundedApprovalPrompt(prefix, preview string, previewTruncated bool) string {
	const suffix = "\nРазрешить один раз?"
	truncation := ""
	if previewTruncated {
		truncation = "\n…\nОписание сокращено."
	}
	prompt := prefix + "\n" + preview + truncation + suffix
	if utf8.RuneCountInString(prompt) <= maximumApprovalPromptRunes && len(prompt) <= maximumApprovalPromptBytes {
		return prompt
	}
	truncation = "\n…\nОписание сокращено."
	fixed := prefix + "\n" + truncation + suffix
	remainingRunes := maximumApprovalPromptRunes - utf8.RuneCountInString(fixed)
	remainingBytes := maximumApprovalPromptBytes - len(fixed)
	preview = truncateUTF8Limits(preview, remainingRunes, remainingBytes)
	return prefix + "\n" + preview + truncation + suffix
}

func truncateUTF8Limits(value string, maximumRunes, maximumBytes int) string {
	if maximumRunes <= 0 || maximumBytes <= 0 {
		return ""
	}
	end, count := 0, 0
	for index, character := range value {
		width := utf8.RuneLen(character)
		if count == maximumRunes || index+width > maximumBytes {
			break
		}
		end = index + width
		count++
	}
	return value[:end]
}

func sanitizeVisibleText(value string) string {
	value = ansiEscapePattern.ReplaceAllString(value, "")
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || character >= ' ' && character != '\u007f' {
			return character
		}
		return -1
	}, value)
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid JSON object")
	}
	return nil
}

func prepareWorkspaceRoot(root string) error {
	if filepath.Clean(root) != root {
		return errors.New("cursor workspace root is invalid")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return errors.New("cursor workspace root is unavailable")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("cursor workspace root is unsafe")
	}
	return nil
}

func (adapter *Adapter) prepareWorkspace(dialogID string) (string, error) {
	if !uuidPattern.MatchString(dialogID) {
		return "", errors.New("cursor dialog workspace identifier is invalid")
	}
	root := filepath.Clean(adapter.config.WorkingDir)
	workspace := filepath.Join(root, dialogID)
	if filepath.Dir(workspace) != root {
		return "", errors.New("cursor dialog workspace escapes root")
	}
	if err := os.Mkdir(workspace, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", errors.New("cursor dialog workspace is unavailable")
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return "", errors.New("cursor dialog workspace is unsafe")
	}
	return workspace, nil
}

func pathsOverlap(first, second string) bool {
	return containsPath(filepath.Clean(first), filepath.Clean(second)) || containsPath(filepath.Clean(second), filepath.Clean(first))
}

func reservedWorkspacePath(root string) bool {
	for _, reserved := range []string{"/auth", "/config", "/state"} {
		if pathsOverlap(root, reserved) {
			return true
		}
	}
	return false
}

func canonicalPathsDisjoint(first, second string) bool {
	canonicalFirst, firstErr := filepath.EvalSymlinks(first)
	canonicalSecond, secondErr := filepath.EvalSymlinks(second)
	return firstErr == nil && secondErr == nil && !pathsOverlap(canonicalFirst, canonicalSecond)
}

func canonicalWorkspacePathAllowed(root string) bool {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	return err == nil && !reservedWorkspacePath(canonicalRoot)
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
