#!/usr/bin/env bash
# Fake stream-json CLI emitting reasoning, a tool call/result, and usage.
echo '{"type":"system","subtype":"init","session_id":"sess-t"}'
echo '{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"let me think"}]},"session_id":"sess-t"}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},"session_id":"sess-t"}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"session_id":"sess-t"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"session_id":"sess-t"}'
echo '{"type":"result","subtype":"success","result":"done","session_id":"sess-t","is_error":false,"total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":3}}'
