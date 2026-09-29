# Local test image for the end-to-end suite: check-jsonschema plus the pinned
# Sourcemeta jsonschema CLI, both run with --network none. The harness copies
# .tools/bin/jsonschema into the build context next to this file; the image is
# never published.
#
# The Python jsonschema package installs a deprecated CLI of the same name,
# so the Sourcemeta binary is placed in /usr/local/bin, which comes first on
# PATH, and the virtual environment's bin directory comes after it.
# droast global ignore=DF012,DF022 reason="a one-shot validator image runs no network service to health-check or expose" expires=2026-12-20
FROM python:3.14-slim@sha256:caaf356f40667c496d405780745b9ac25771c189a51dfcc42430d531ea09f8a2 AS build

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_ROOT_USER_ACTION=ignore \
    PYTHONDONTWRITEBYTECODE=1

COPY requirements.txt /tmp/requirements.txt

RUN python -m venv /opt/validators \
    && /opt/validators/bin/pip install --no-cache-dir --require-hashes --only-binary=:all: --no-deps -r /tmp/requirements.txt \
    && /opt/validators/bin/check-jsonschema --version

FROM python:3.14-slim@sha256:caaf356f40667c496d405780745b9ac25771c189a51dfcc42430d531ea09f8a2

ENV PATH=/usr/local/bin:/opt/validators/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

COPY --from=build /opt/validators /opt/validators
COPY jsonschema /usr/local/bin/jsonschema

RUN ln -s /opt/validators/bin/check-jsonschema /usr/local/bin/check-jsonschema \
    && check-jsonschema --version \
    && jsonschema version

WORKDIR /work
USER 65534:65534
CMD ["check-jsonschema", "--version"]
