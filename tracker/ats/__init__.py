"""ATS provider adapters. Each exposes fetch(slug) -> FetchResult."""
from dataclasses import dataclass, field

PROVIDERS = ("greenhouse", "lever", "ashby")


@dataclass
class NormalizedJob:
    """One posting, provider-shaped fields flattened. Everything optional except
    the three that identify it — adapters must never raise on a missing field."""
    external_id: str
    title: str
    url: str
    source: str                       # 'greenhouse' | 'lever' | 'ashby'
    posted_at: str | None = None      # ISO-8601 date/datetime
    location: str | None = None
    employment_type: str | None = None
    remote: bool | None = None
    jd_text: str = ""
    pay_min: float | None = None
    pay_max: float | None = None
    pay_currency: str | None = None
    raw: dict = field(default_factory=dict)


@dataclass
class FetchResult:
    """Adapter outcome plus the transport facts poll_health needs. `jobs` is None
    on failure and [] on a genuinely empty board — the difference is the whole
    point of feed-death detection, so it must survive to the caller."""
    jobs: list[NormalizedJob] | None
    status: int | None = None
    error: str | None = None
    not_modified: bool = False
    etag: str | None = None
    last_modified: str | None = None

    @property
    def ok(self) -> bool:
        return self.error is None and self.jobs is not None


def adapter(name: str):
    from . import ashby, greenhouse, lever

    return {"greenhouse": greenhouse, "lever": lever, "ashby": ashby}.get(name)
