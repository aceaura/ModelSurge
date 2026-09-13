#!/bin/sh
set -eu
chown -R modelsurge:modelsurge /data
exec su-exec modelsurge "$@"
