FROM eclipse-temurin:21-jdk-alpine

# Groovy is distributed as a JVM-based tool. eclipse-temurin:21-jdk-alpine is
# multi-arch (arm64 + amd64); we install Groovy from the upstream archive so
# the resulting image works natively on both architectures.
RUN apk add --no-cache bash curl unzip

ARG GROOVY_VERSION=4.0.15
RUN curl -fsSL -o /tmp/groovy.zip \
        "https://archive.apache.org/dist/groovy/${GROOVY_VERSION}/distribution/apache-groovy-binary-${GROOVY_VERSION}.zip" \
    && unzip -q /tmp/groovy.zip -d /opt \
    && mv "/opt/groovy-${GROOVY_VERSION}" /opt/groovy \
    && rm /tmp/groovy.zip

ENV PATH="/opt/groovy/bin:${PATH}" \
    GROOVY_HOME="/opt/groovy"

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN groovy --version && echo "Groovy runtime OK"
