import json
import os
import socket
import sys


def _block_network():
    def blocked(self, address, *args, **kwargs):
        raise OSError("network blocked by the okx step fixture: %r" % (address,))

    socket.socket.connect = blocked
    socket.socket.connect_ex = blocked


def main():
    _block_network()
    with open(os.environ["OKX_STEP_CHECK_FIXTURE"]) as fh:
        spec = json.load(fh)
    log = spec.get("argv_log")
    if log:
        with open(log, "a") as fh:
            fh.write(json.dumps(sys.argv[1:]) + "\n")
    if spec.get("stderr"):
        sys.stderr.write(spec["stderr"])
    if "raw_stdout" in spec:
        sys.stdout.write(spec["raw_stdout"])
    else:
        sys.stdout.write(json.dumps(spec.get("response", {})))
    sys.stdout.flush()
    sys.exit(int(spec.get("exit_code", 0)))


if __name__ == "__main__":
    main()
