#!/usr/bin/env bash
# Minimal ACP agent for tests. It implements initialize and session/new, then a
# single prompt turn that requests permission, streams a thought and two message
# chunks, and ends the turn with a stop reason and usage.
set -euo pipefail

id_of() { printf '%s' "$1" | sed -E 's/.*"id":([^,}]+).*/\1/'; }

read -r line # initialize
id=$(id_of "$line")
printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[],"agentInfo":{"name":"fake","version":"0"}}}\n' "$id"

read -r line # session/new or session/load
id=$(id_of "$line")
if [ -n "${FAKE_ACP_LOG:-}" ]; then printf '%s\n' "$line" >>"$FAKE_ACP_LOG"; fi
printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_fake"}}\n' "$id"

read -r line # session/prompt
id=$(id_of "$line")

# Ask for permission and wait for the client's answer.
printf '{"jsonrpc":"2.0","id":9001,"method":"session/request_permission","params":{"sessionId":"ses_fake","toolCall":{"toolCallId":"tc1","title":"run tests","kind":"execute"},"options":[{"optionId":"allow_once","name":"Allow","kind":"allow_once"},{"optionId":"reject_once","name":"Reject","kind":"reject_once"}]}}\n'
read -r answer

printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}}\n'
printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello"}}}}\n'
printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" world"}}}}\n'
printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":7,"outputTokens":2,"totalTokens":9}}}\n' "$id"

# Stay alive until the client stops the process at end of turn.
sleep 10
