#!/usr/bin/env bash
# Fake codebuddy: emits claude/codebuddy-style stream-json NDJSON.
# Logs its argv to $HARBOR_ARG_LOG (for resume assertions) when set.
if [ -n "$HARBOR_ARG_LOG" ]; then
  echo "$@" >> "$HARBOR_ARG_LOG"
fi

echo '{"type":"system","subtype":"init","session_id":"sess-123"}'
sleep 0.05
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Hello "}]},"session_id":"sess-123"}'
sleep 0.05
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]},"session_id":"sess-123"}'
echo '{"type":"result","subtype":"success","result":"Hello world","session_id":"sess-123","is_error":false}'
