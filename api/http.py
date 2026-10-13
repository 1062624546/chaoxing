"""Bounded transport; never retry access denials or replay POST requests."""
import threading
from urllib.parse import urlsplit
import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from api.exceptions import AccessDenied, SessionExpired, ResponseFormatError, RemoteHTTPError

class ManagedSession(requests.Session):
    def __init__(self):
        super().__init__()
        self.lock = threading.RLock()
        self.max_redirects = 5
        retries = Retry(total=2, connect=2, read=2, status=0, other=0,
                        backoff_factor=0.5, allowed_methods=frozenset({'GET', 'HEAD'}))
        for scheme in ('https://', 'http://'):
            self.mount(scheme, HTTPAdapter(max_retries=retries, pool_connections=4, pool_maxsize=4))

    def request(self, method, url, **kwargs):
        kwargs.setdefault('timeout', (5, 20))
        with self.lock:
            return super().request(method, url, **kwargs)


def check_response(response):
    """Keep messages free of URL query strings, credentials and response bodies."""
    if response.status_code == 403:
        raise AccessDenied('HTTP 403：平台拒绝访问，请在官方网页检查；不自动重试')
    if response.status_code == 401 or urlsplit(response.url).hostname == 'passport2.chaoxing.com':
        raise SessionExpired('登录会话已失效，请更新 Cookie 或重新登录')
    if 400 <= response.status_code < 500:
        raise AccessDenied(f'HTTP {response.status_code}：请求被拒绝或参数不兼容；停止自动重试')
    if response.status_code != 200:
        raise RemoteHTTPError(f'HTTP {response.status_code}：请求失败')
    # Only explicit login forms, never a generic "login" substring.
    if 'id="loginBtn"' in response.text or 'id="formLogin"' in response.text:
        raise SessionExpired('返回登录页面，请更新 Cookie')
