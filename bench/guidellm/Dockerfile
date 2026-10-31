# GuideLLM load generator image. Build once, reuse for every run:
#   docker build -t marbor-guidellm:0.8.1 bench/guidellm
FROM python:3.12-slim
ARG GUIDELLM_VERSION=0.8.1
ENV PIP_DEFAULT_TIMEOUT=120 PIP_RETRIES=10 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN pip install --no-cache-dir "guidellm==${GUIDELLM_VERSION}"
ENTRYPOINT ["guidellm"]
