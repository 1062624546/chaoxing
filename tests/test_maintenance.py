import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
import requests
from requests.cookies import create_cookie
from api.base import Chaoxing, SessionManager
from api.config import GlobalConst
from api.cookies import save_cookies, use_cookies
from api.http import ManagedSession, check_response
from api.exceptions import AccessDenied, SessionExpired, ResponseFormatError
from api.security import redact
import main


def response(status=200, text='mArg={"attachments":[],"defaults":{}};', url='https://mooc1.chaoxing.com/mooc-ans/knowledge/cards'):
    r = requests.Response()
    r.status_code = status
    r._content = text.encode()
    r.url = url
    return r


class MaintenanceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path_patch = patch.object(GlobalConst, 'COOKIES_PATH', str(Path(self.tmp.name)/'cookies.txt'))
        self.path_patch.start()
        SessionManager._instance = None

    def tearDown(self):
        if SessionManager._instance:
            SessionManager.get_session().close()
        SessionManager._instance = None
        self.path_patch.stop()
        self.tmp.cleanup()

    def test_session_identity_and_live_cookie(self):
        s = SessionManager.get_session()
        s.cookies.set('test', 'value', domain='.chaoxing.com')
        self.assertIs(s, SessionManager.get_session())
        self.assertEqual(SessionManager.get_session().cookies.get('test'), 'value')

    def test_cookie_import_bom_prefix_equals(self):
        Path(GlobalConst.COOKIES_PATH).write_text('\ufeffCookie: _uid=123; token=a=b;', encoding='utf-8')
        jar = use_cookies()
        self.assertEqual(jar.get('token'), 'a=b')
        self.assertTrue(all(c.domain == '.chaoxing.com' for c in jar))

    def test_cookie_roundtrip_duplicate_names_scopes_and_expiry(self):
        s = requests.Session()
        s.cookies.set_cookie(create_cookie('x', 'one', domain='mooc1.chaoxing.com', path='/'))
        s.cookies.set_cookie(create_cookie('x', 'two', domain='.chaoxing.com', path='/mooc-ans', secure=True))
        s.cookies.set_cookie(create_cookie('old', 'expired', domain='.chaoxing.com', expires=1))
        save_cookies(s)
        jar = use_cookies()
        self.assertEqual(len(jar), 2)
        self.assertEqual({c.value for c in jar}, {'one', 'two'})
        self.assertTrue(any(c.secure and c.path == '/mooc-ans' for c in jar))
        if os.name != 'nt':
            self.assertEqual(Path(GlobalConst.COOKIES_PATH).stat().st_mode & 0o777, 0o600)

    def test_invalid_cookie_not_silent(self):
        Path(GlobalConst.COOKIES_PATH).write_text('invalid')
        with self.assertRaises(ValueError): use_cookies()

    def test_cookie_reload_replaces_old(self):
        s = SessionManager.get_session()
        s.cookies.set('stale', 'value')
        Path(GlobalConst.COOKIES_PATH).write_text('_uid=123')
        SessionManager.update_cookies()
        self.assertNotIn('stale', s.cookies)

    def test_403_raises_and_does_not_fetch_more_cards(self):
        s = Mock()
        s.get.return_value = response(403, '<h1>Forbidden</h1>')
        cx = Chaoxing()
        cx.rate_limiter = Mock()
        with patch.object(SessionManager, 'get_session', return_value=s):
            with self.assertRaises(AccessDenied):
                cx.get_job_list({'clazzId':'1','courseId':'2','cpi':'3'}, {'id':'4'})
        self.assertEqual(s.get.call_count, 1)

    def test_403_chapter_is_blocked(self):
        cx = Mock()
        cx.get_job_list.side_effect = AccessDenied('403')
        self.assertEqual(main.process_chapter(cx, {}, {'title':'test','has_finished':False},1), main.ChapterResult.BLOCKED)

    def test_timeout_chapter_is_retryable(self):
        cx = Mock()
        cx.get_job_list.side_effect = requests.Timeout()
        self.assertEqual(main.process_chapter(cx, {}, {'title':'test','has_finished':False},1), main.ChapterResult.ERROR)

    def test_blocked_worker_finishes_without_retry_or_success(self):
        cx = Mock()
        cx.get_job_list.side_effect = AccessDenied('403')
        task = main.ChapterTask(0, {'title':'test','has_finished':False}, {'title':'course'})
        proc = main.JobProcessor(cx, [task], {'speed':1,'jobs':1,'notopen_action':'retry'})
        from tests.test_job_processor import JobProcessorTestCase
        JobProcessorTestCase()._run_with_timeout(proc)
        self.assertEqual(task.result, main.ChapterResult.BLOCKED)
        self.assertEqual(task.tries, 0)
        self.assertIn(task, proc.failed_tasks)
        self.assertEqual(proc.task_queue.unfinished_tasks, 0)

    def test_login_form_and_generic_login_word(self):
        with self.assertRaises(SessionExpired): check_response(response(text='<button id="loginBtn">'))
        check_response(response(text='course login instructions'))

    def test_validation_403_is_not_expiry(self):
        s = SessionManager.get_session()
        s.cookies.set('_uid','123',domain='.chaoxing.com')
        with patch.object(s,'post',return_value=response(403)):
            with self.assertRaises(AccessDenied): Chaoxing()._validate_cookie_session()

    def test_unknown_cards_fail_closed(self):
        s = Mock()
        s.get.return_value = response(text='<html>Unknown</html>')
        cx = Chaoxing(); cx.rate_limiter = Mock()
        with patch.object(SessionManager, 'get_session', return_value=s):
            with self.assertRaises(ResponseFormatError):
                cx.get_job_list({'clazzId':'1','courseId':'2','cpi':'3'}, {'id':'4'})

    def test_retry_bounds_and_post_exclusion(self):
        s = ManagedSession()
        retry = s.get_adapter('https://').max_retries
        self.assertEqual(retry.total, 2)
        self.assertEqual(retry.status, 0)
        self.assertNotIn('POST', retry.allowed_methods)

    def test_redaction(self):
        raw = 'https://example.com/secret?token=abcdef password=secret Cookie: session=hidden'
        clean = redact(raw)
        for secret in ('abcdef','secret','hidden'):
            self.assertNotIn(secret, clean)

    def test_decoder_preserves_text_whitespace(self):
        from api.decode import decode_course_card
        jobs, info = decode_course_card('mArg = {"attachments": [], "defaults": {}};')
        self.assertEqual(jobs, [])

    def test_cookie_login_does_not_fallback_to_password(self):
        from api.base import Account
        cx = Chaoxing(account=Account('fake', 'fake'))
        with patch.object(cx, '_validate_cookie_session', return_value=False), patch('api.base.ManagedSession') as transport:
            self.assertFalse(cx.login(login_with_cookies=True)['status'])
            transport.assert_called_once()  # Only SessionManager construction; no password session.

    def test_password_redirect_stops_after_one_request(self):
        from api.base import Account
        cx = Chaoxing(account=Account('fake', 'fake'))
        with patch('api.base.ManagedSession') as transport:
            transport.return_value.post.return_value = response(302)
            self.assertFalse(cx.login()['status'])
            transport.return_value.post.assert_called_once()
            self.assertFalse(transport.return_value.post.call_args.kwargs['allow_redirects'])

    def test_official_empty_cards_are_supported(self):
        s = Mock()
        s.get.return_value = response(text='<div>暂无内容</div>')
        cx = Chaoxing(); cx.rate_limiter = Mock()
        with patch.object(SessionManager, 'get_session', return_value=s), patch.object(cx,'study_emptypage',return_value=main.StudyResult.SUCCESS), patch('api.base.save_cookies'):
            self.assertEqual(cx.get_job_list({'clazzId':'1','courseId':'2','cpi':'3'},{'id':'4'}), ([],{}))
        self.assertEqual(s.get.call_count,1)
        params = s.get.call_args.kwargs['params']
        self.assertEqual(params['isMicroCourse'],'false')
        self.assertEqual(params['editorPreview'],0)

    def test_empty_page_failure_not_success(self):
        s = Mock(); s.get.return_value = response(text='<div>暂无内容</div>')
        cx = Chaoxing(); cx.rate_limiter = Mock()
        with patch.object(SessionManager,'get_session',return_value=s), patch.object(cx,'study_emptypage',return_value=main.StudyResult.ERROR):
            with self.assertRaises(ResponseFormatError):
                cx.get_job_list({'clazzId':'1','courseId':'2','cpi':'3'},{'id':'4'})

    def test_real_transport_does_not_retry_403(self):
        import threading
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
        calls = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(1)
                self.send_response(403)
                self.end_headers()
            def log_message(self, *args): pass
        server = ThreadingHTTPServer(('127.0.0.1',0),Handler)
        t = threading.Thread(target=server.serve_forever,daemon=True); t.start()
        try:
            with ManagedSession() as s:
                s.trust_env = False
                r = s.get(f'http://127.0.0.1:{server.server_port}/cards')
                self.assertEqual(r.status_code,403)
            self.assertEqual(len(calls),1)
        finally:
            server.shutdown(); server.server_close(); t.join()

    def test_atomic_cookie_failure_preserves_original(self):
        Path(GlobalConst.COOKIES_PATH).write_text('_uid=original')
        s = requests.Session(); s.cookies.set('_uid','new',domain='.chaoxing.com')
        with patch('api.cookies.os.replace',side_effect=OSError('disk error')):
            with self.assertRaises(OSError): save_cookies(s)
        self.assertEqual(use_cookies().get('_uid'),'original')
        self.assertEqual(list(Path(self.tmp.name).glob('.cookies-*')),[])

    def test_duplicate_identity_with_same_value(self):
        s = SessionManager.get_session()
        s.cookies.set('_uid','123',domain='.chaoxing.com')
        s.cookies.set('_uid','123',domain='mooc1.chaoxing.com')
        self.assertEqual(Chaoxing().get_uid(),'123')
        s.cookies.set('_uid','456',domain='mooc1.chaoxing.com')
        with self.assertRaises(SessionExpired): Chaoxing().get_uid()

    def test_percent_credentials_and_config_bom(self):
        path = Path(self.tmp.name)/'config.ini'
        path.write_text('\ufeff[common]\npassword=example%value\njobs=1\n[tiku]\n',encoding='utf-8')
        common, _, _ = main.load_config_from_file(path)
        self.assertEqual(common['password'],'example%value')
