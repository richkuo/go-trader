import os
import sys
import time

if "--probe-only" in sys.argv:
    print("{}")
    sys.exit(0)

os.fork()
time.sleep(600)
