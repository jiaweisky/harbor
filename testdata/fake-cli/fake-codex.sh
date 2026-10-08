#!/usr/bin/env bash
# Fake codex: emits `codex exec --json` style JSONL events.
# Logs its argv to $HARBOR_ARG_LOG (for attachment assertions) when set.
if [ -n "$HARBOR_ARG_LOG" ]; then
  echo "$@" >> "$HARBOR_ARG_LOG"
fi
echo '{"type":"thread.started","thread_id":"t-1"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Hello from codex"}}'
echo '{"type":"turn.completed"}'
