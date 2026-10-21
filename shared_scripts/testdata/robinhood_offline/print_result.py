import os
import sys

sys.stdout.write(os.environ.get("RH_FIXTURE_STDOUT", ""))
sys.stdout.flush()
sys.exit(int(os.environ.get("RH_FIXTURE_EXIT", "0")))
