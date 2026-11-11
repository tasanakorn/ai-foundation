"""Dev loop for the monorepo. Standard library only.

Runs, with prefixed output:
  gateway  Go gateway. Rebuilds and restarts when gateway/**/*.go or *.yaml change.
           A failed build keeps the last good process running.
  web      Nuxt dev server (own hot reload). Proxies /admin/v1 to the gateway.
  llama    llama-server, only if LLAMA_MODEL (path to .gguf) or LLAMA_HF (HF repo) is set.

Environment: read from the shell, then from .env at the repo root (shell wins).

Usage:
  python scripts/dev.py [--no-web] [--no-llama]
"""

import argparse
import os
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GATEWAY = ROOT / "gateway"
WEB = ROOT / "web"
DEV = ROOT / ".dev"

COLORS = {"dev": "32", "gateway": "36", "web": "35", "llama": "33"}
print_lock = threading.Lock()


def log(name: str, text: str) -> None:
    with print_lock:
        for line in text.rstrip("\n").splitlines() or [""]:
            sys.stdout.write(f"\033[{COLORS[name]}m{name:>7}\033[0m | {line}\n")
        sys.stdout.flush()


def load_env() -> dict[str, str]:
    env = dict(os.environ)
    dotenv = ROOT / ".env"
    if dotenv.exists():
        for raw in dotenv.read_text().splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env.setdefault(k.strip(), v.strip().strip("'\""))
    env.setdefault("AIF_MASTER_KEY", "dev-master-key-0000")
    env.setdefault("AIF_DB", str(DEV / "aif.db"))
    return env


class Proc:
    """A child process in its own process group, with output piped to log()."""

    def __init__(self, name: str, cmd: list[str], cwd: Path, env: dict[str, str]):
        self.name, self.cmd, self.cwd, self.env = name, cmd, cwd, env
        self.p: subprocess.Popen | None = None

    def start(self) -> None:
        self.p = subprocess.Popen(
            self.cmd, cwd=self.cwd, env=self.env, text=True, bufsize=1,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True,
        )
        threading.Thread(target=self._pump, args=(self.p,), daemon=True).start()

    def _pump(self, p: subprocess.Popen) -> None:
        assert p.stdout
        for line in p.stdout:
            log(self.name, line)
        code = p.wait()
        if code not in (0, -signal.SIGTERM, -signal.SIGINT):
            log(self.name, f"exited with code {code}")

    def stop(self) -> None:
        if not self.p or self.p.poll() is not None:
            return
        try:
            os.killpg(self.p.pid, signal.SIGTERM)
            self.p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(self.p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def gateway_snapshot() -> dict[Path, int]:
    out = {}
    for p in GATEWAY.rglob("*"):
        if p.suffix in (".go", ".yaml", ".mod", ".sum") and p.is_file() and "bin" not in p.parts:
            out[p] = p.stat().st_mtime_ns
    return out


def build_gateway(env: dict[str, str]) -> bool:
    t0 = time.monotonic()
    r = subprocess.run(
        ["go", "build", "-o", str(DEV / "aif"), "./cmd/aif"],
        cwd=GATEWAY, env=env, capture_output=True, text=True,
    )
    if r.returncode != 0:
        log("gateway", "build failed (keeping the last good build running):\n" + r.stderr)
        return False
    log("gateway", f"built in {time.monotonic() - t0:.1f}s")
    return True


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--no-web", action="store_true", help="do not start the Nuxt dev server")
    ap.add_argument("--no-llama", action="store_true", help="do not start llama-server")
    args = ap.parse_args()

    DEV.mkdir(exist_ok=True)
    env = load_env()
    procs: list[Proc] = []

    # llama-server (optional)
    if not args.no_llama and (env.get("LLAMA_MODEL") or env.get("LLAMA_HF")):
        src = ["-m", env["LLAMA_MODEL"]] if env.get("LLAMA_MODEL") else ["-hf", env["LLAMA_HF"]]
        extra = env.get("LLAMA_ARGS", "").split()
        llama = Proc("llama", ["llama-server", *src, "--host", "127.0.0.1", "--port", "8090",
                               "--alias", "local", *extra], ROOT, env)
        llama.start()
        procs.append(llama)
    elif not args.no_llama:
        log("dev", "llama-server not started (set LLAMA_MODEL=/path/model.gguf or LLAMA_HF=<repo> in .env)")

    # web
    if not args.no_web:
        if not (WEB / "node_modules").exists():
            log("dev", "installing web dependencies (pnpm install)...")
            subprocess.run(["pnpm", "install"], cwd=WEB, env=env, check=True)
        web = Proc("web", ["pnpm", "dev"], WEB, env)
        web.start()
        procs.append(web)

    # gateway
    gw = Proc("gateway", [str(DEV / "aif"), "serve", "-config", "aif.dev.yaml"], GATEWAY, env)
    if build_gateway(env):
        gw.start()
    procs.append(gw)

    log("dev", "public API  http://127.0.0.1:8080/v1")
    log("dev", "admin API   http://127.0.0.1:8081/admin/v1")
    if not args.no_web:
        log("dev", "admin UI    http://localhost:3000")
    log("dev", f"master key  {env['AIF_MASTER_KEY']}")
    if not env.get("OPENROUTER_API_KEY"):
        log("dev", "OPENROUTER_API_KEY is not set: openrouter models will fail")

    stop = threading.Event()
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    signal.signal(signal.SIGTERM, lambda *_: stop.set())

    snap = gateway_snapshot()
    while not stop.wait(0.5):
        now = gateway_snapshot()
        if now == snap:
            continue
        time.sleep(0.3)  # let editors finish writing
        snap = gateway_snapshot()
        log("gateway", "change detected, rebuilding...")
        if build_gateway(env):
            gw.stop()
            gw.start()

    log("dev", "stopping...")
    for p in reversed(procs):
        p.stop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
