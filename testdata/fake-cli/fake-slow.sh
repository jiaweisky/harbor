#!/usr/bin/env bash
# Fake slow codebuddy for cancellation tests.
echo '{"type":"system","subtype":"init","session_id":"slow-1"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"slow-1"}'
sleep 30
echo '{"type":"result","subtype":"success","result":"done","session_id":"slow-1","is_error":false}'
