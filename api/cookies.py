"""Cookie header import and atomic, domain-aware persistence."""
from contextlib import nullcontext
import json
import os
import tempfile
import threading
from pathlib import Path
import requests
from requests.cookies import create_cookie
from api.config import GlobalConst as gc

cookie_lock = threading.RLock()


def save_cookies(session):
    with getattr(session, "lock", nullcontext()), cookie_lock:
        path = Path(gc.COOKIES_PATH)
        path.parent.mkdir(parents=True, exist_ok=True)
        records = [{k: getattr(c, k) for k in
                    ('name', 'value', 'domain', 'path', 'secure', 'expires', 'discard',
                     'domain_specified', 'domain_initial_dot', '_rest')}
                   for c in session.cookies if not c.is_expired()]
        fd, tmp = tempfile.mkstemp(dir=path.parent, prefix='.cookies-')
        try:
            with os.fdopen(fd, 'w', encoding='utf-8') as f:
                json.dump(records, f, ensure_ascii=False)
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp, path)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)


def use_cookies():
    with cookie_lock:
        path = Path(gc.COOKIES_PATH)
        jar = requests.cookies.RequestsCookieJar()
        if not path.exists():
            return jar
        # Permission errors and malformed files are actionable, not empty sessions.
        raw = path.read_text(encoding='utf-8-sig').strip()
        if not raw:
            return jar
        if raw.startswith('['):
            try:
                for record in json.loads(raw):
                    domain = record.get('domain', '')
                    if domain.lstrip('.') != 'chaoxing.com' and not domain.endswith('.chaoxing.com'):
                        raise ValueError()
                    record = dict(record)
                    domain_specified = record.pop('domain_specified', bool(domain))
                    domain_initial_dot = record.pop('domain_initial_dot', domain.startswith('.'))
                    rest = record.pop('_rest', {})
                    cookie = create_cookie(**record, rest=rest)
                    cookie.domain_specified = domain_specified
                    cookie.domain_initial_dot = domain_initial_dot
                    if not cookie.is_expired():
                        jar.set_cookie(cookie)
            except (ValueError, TypeError, KeyError):
                raise ValueError('Cookie JSON 文件格式或域名无效') from None
        else:
            if raw.lower().startswith('cookie:'):
                raw = raw.split(':', 1)[1].strip()
            for item in raw.split(';'):
                if not item.strip():
                    continue
                name, sep, value = item.strip().partition('=')
                if not sep or not name or any(c.isspace() for c in name):
                    raise ValueError('Cookie 文件格式无效，应为 name=value; name=value')
                jar.set(name, value, domain='.chaoxing.com', path='/')
        return jar
