"""Desktop notification + optional phone push. Never fatal: a failed notify
must not stop the rest of the run."""
import os
import shutil
import subprocess

NTFY_TOPIC_ENV = "NTFY_TOPIC"
NTFY_SERVER = os.environ.get("NTFY_SERVER", "https://ntfy.sh")


def _load_env() -> None:
    try:
        from dotenv import load_dotenv

        load_dotenv(
            os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), ".env")
        )
    except Exception:
        pass


def desktop(title: str, body: str, urgency: str = "normal") -> bool:
    if not shutil.which("notify-send"):
        return False
    try:
        subprocess.run(
            ["notify-send", "-u", urgency, "-a", "Job Tracker", title, body],
            check=True, timeout=10,
        )
        return True
    except Exception:
        return False


def phone(message: str, title: str = "Job Tracker") -> bool:
    _load_env()
    topic = os.environ.get(NTFY_TOPIC_ENV, "").strip()
    if not topic:
        return False
    try:
        import requests

        requests.post(
            f"{NTFY_SERVER.rstrip('/')}/{topic}",
            data=message.encode("utf-8"),
            headers={"Title": title},
            timeout=10,
        )
        return True
    except Exception:
        return False


def send(title: str, body: str, urgency: str = "normal") -> None:
    desktop(title, body, urgency)
    phone(f"{title}\n{body}", title="Job Tracker")
