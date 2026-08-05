import json
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

FIXTURES = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fixtures")


def fixture(name):
    with open(os.path.join(FIXTURES, name), encoding="utf-8") as fh:
        return json.load(fh)


@pytest.fixture
def conn(tmp_path):
    """A real SQLite database, per test, on disk under tmp_path."""
    from tracker import db

    connection = db.connect(str(tmp_path / "test.db"))
    yield connection
    connection.close()


@pytest.fixture(autouse=True)
def no_notifications(monkeypatch):
    """Tests must never fire a desktop notification or touch the network."""
    from tracker import notify

    sent = []
    monkeypatch.setattr(notify, "send", lambda *a, **k: sent.append((a, k)))
    monkeypatch.setattr(notify, "desktop", lambda *a, **k: False)
    monkeypatch.setattr(notify, "phone", lambda *a, **k: False)
    return sent


@pytest.fixture(autouse=True)
def no_network(monkeypatch):
    """Any accidental live request fails loudly rather than silently passing CI."""
    def blocked(*a, **k):
        raise AssertionError("test attempted a live HTTP request")

    monkeypatch.setattr("requests.get", blocked, raising=False)
    monkeypatch.setattr("requests.post", blocked, raising=False)
