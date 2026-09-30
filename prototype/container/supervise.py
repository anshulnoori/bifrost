"""Fail-stop supervision under tini; bounded shutdown; no raw child logs."""
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.request


def main():
    stop = threading.Event()
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, lambda *_: stop.set())
    if not os.environ.get('TS_AUTHKEY'):
        print('enrollment_unconfigured', flush=True)
        return 1
    ts_env = dict(os.environ)
    os.environ.pop('TS_AUTHKEY', None)
    children = []
    result = 0
    try:
        for command, env in [(['/usr/local/bin/containerboot'], ts_env),
                             ([sys.executable, '/app/probe.py'], dict(os.environ))]:
            children.append(subprocess.Popen(command, env=env, start_new_session=True,
                                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        ts_env.clear()
        deadline = time.monotonic() + 90
        ready = False
        while not stop.wait(1):
            if any(child.poll() is not None for child in children):
                print('child_exited', flush=True)
                result = 1
                break
            try:
                with urllib.request.urlopen('http://127.0.0.1:9002/healthz', timeout=2) as response:
                    healthy = response.status == 200
            except Exception:
                healthy = False
            if healthy:
                if not ready:
                    print('tailnet_ip_ready', flush=True)
                    ready = True
                deadline = time.monotonic() + 90
            elif time.monotonic() > deadline:
                print('tailnet_health_timeout', flush=True)
                result = 1
                break
    finally:
        # containerboot forwards TERM to tailscaled; mem: state logs out on clean exit.
        for child in children:
            if child.poll() is None:
                os.killpg(child.pid, signal.SIGTERM)
        deadline = time.monotonic() + 10
        for child in children:
            try:
                child.wait(timeout=max(0.01, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait()
        print('supervisor_stopped', flush=True)
    return result


if __name__ == '__main__':
    sys.exit(main())
