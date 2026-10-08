#!/usr/bin/env bash
# Fake container runtime for tests: record the argv, then exec the command that
# follows the image token so the wrapped CLI actually runs.
printf '%s\n' "$@" >>"$FAKE_DOCKER_LOG"
while [ $# -gt 0 ]; do
  if [ "$1" = "$FAKE_DOCKER_IMAGE" ]; then
    shift
    exec "$@"
  fi
  shift
done
echo "fake-docker: image $FAKE_DOCKER_IMAGE not found" >&2
exit 1
