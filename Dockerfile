FROM python:3.13-slim
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 \
    CHAOXING_COOKIES_PATH=/config/cookies.txt CHAOXING_LOG_PATH=/logs/chaoxing.log
WORKDIR /app
COPY requirements.lock.txt ./
ARG PIP_INDEX_URL=https://pypi.org/simple
RUN pip install --no-cache-dir -r requirements.lock.txt --index-url "$PIP_INDEX_URL" \
    && groupadd --gid 10001 chaoxing \
    && useradd --uid 10001 --gid 10001 --no-create-home chaoxing \
    && mkdir /config /logs && chown 10001:10001 /config /logs /app
COPY --chown=10001:10001 api ./api
COPY --chown=10001:10001 resource ./resource
COPY --chown=10001:10001 main.py config_template.ini ./
USER 10001:10001
ENTRYPOINT ["python", "main.py", "-c", "/config/config.ini"]
