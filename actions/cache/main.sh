#!/bin/sh
# No-op at runtime. The drawbar controller reads INPUT_KEY, INPUT_PATH,
# and INPUT_RESTORE-KEYS from the step environment at job-build time
# and sets up ZFS snapshot bind mounts before the job starts.
echo "drawbar/cache: paths bind-mounted by controller"
