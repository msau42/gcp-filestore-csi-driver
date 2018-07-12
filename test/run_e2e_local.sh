#!/bin/bash

set -e
set -x

mydir=$(dirname $0)
source "$mydir/../deploy/common.sh"

go run "$mydir/remote/run_remote/run_remote.go" --logtostderr --v 2 --project "${PROJECT}" --zone "${ZONE}" --ssh-env gce --delete-instances=false --cleanup=false --results-dir=my_test  --service-account="${IAM_NAME}"
