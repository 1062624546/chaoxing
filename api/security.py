"""Conservative log scrubbing; never use redaction as permission to log secrets."""
import re

def redact(text):
    text = re.sub(r'https?://[^\s<>\"\']+', '[URL已隐藏]', str(text))
    text = re.sub(r'(?i)((?:cookie|authorization)\s*[:=]\s*)[^\r\n]+', r'\1[已隐藏]', text)
    text = re.sub(r'(?i)([\"\']?(?:password|passwd|token|dtoken|enc|_uid|uid|uname|api_key|key)[\"\']?\s*[:=]\s*)[^,;\s}]+', r'\1[已隐藏]', text)
    return text

def scrub_record(record):
    record['message'] = redact(record['message'])
