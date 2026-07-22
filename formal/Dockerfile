# syntax=docker/dockerfile:1.7
FROM eclipse-temurin:21-jre-jammy

ARG TLA_TOOLS_VERSION=1.7.1
ARG TLA_TOOLS_SHA1=9416f74257aa50f250776db34964db7ec99e9883

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && curl --fail --location --silent --show-error \
       "https://github.com/tlaplus/tlaplus/releases/download/v${TLA_TOOLS_VERSION}/tla2tools.jar" \
       --output /opt/tla2tools.jar \
    && echo "${TLA_TOOLS_SHA1}  /opt/tla2tools.jar" | sha1sum --check --strict - \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /model
ENTRYPOINT ["java", "-XX:+UseParallelGC", "-cp", "/opt/tla2tools.jar", "tlc2.TLC"]
CMD ["-workers", "auto", "-config", "TaskGate.cfg", "TaskGate.tla"]
