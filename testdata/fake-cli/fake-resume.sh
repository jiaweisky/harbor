#!/usr/bin/env bash
# Fake stream-json CLI for resume recovery:
# - first launch: emit a session id and a permission request, then exit (the
#   task pauses on input-required);
# - launched with --resume (client answered after the process was gone): echo a
#   terminal result so the recovered task completes.
case "$*" in
  *--resume*)
    read -r _ # injected answer prompt
    echo '{"type":"result","result":"resumed","is_error":false}'
    exit 0
    ;;
esac
read -r _ # initial user message
echo '{"type":"system","subtype":"init","session_id":"sess-xyz"}'
echo '{"type":"control_request","request_id":"perm_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo hi"}}}'
