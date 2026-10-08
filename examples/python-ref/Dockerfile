# agent-ref as a container image: the Python app and heain-agent in one image (CineNexus Pro case study, step 2a).
# heain-agent is built on the host first (conformance.docker.yaml `build`, CGO off, so it runs on any glibc),
# then copied in. The agent is PID 1's child (docker run --init): it enrols, starts `python3 app.py` and
# stops it on SIGTERM. Every HEAIN_* path is mounted by whoever runs the container (the conformance suite
# mounts its work directory at the same path).
FROM python:3.12-slim-bookworm
LABEL org.opencontainers.image.title="agent-ref" \
      org.opencontainers.image.description="HEAIN Framework reference app in Python, through heain-agent" \
      org.opencontainers.image.source="https://github.com/heainframework/heain-agent"
WORKDIR /app
COPY heain-agent heain-agent.json app.py heain_agent.py ./
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
USER 65534:65534
ENTRYPOINT ["/app/heain-agent"]
