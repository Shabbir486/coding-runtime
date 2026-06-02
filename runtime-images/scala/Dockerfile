FROM eclipse-temurin:21-jdk-alpine

# eclipse-temurin:21-jdk-alpine is multi-arch. The hseeberger/scala-sbt image
# referenced previously is deprecated and uses an older JDK; build Scala from
# the upstream archive on top of Temurin 21 instead.
RUN apk add --no-cache bash curl tar gzip

ARG SCALA_VERSION=3.3.4
RUN curl -fsSL "https://github.com/scala/scala3/releases/download/${SCALA_VERSION}/scala3-${SCALA_VERSION}.tar.gz" \
        -o /tmp/scala.tar.gz \
    && tar -xzf /tmp/scala.tar.gz -C /opt \
    && mv "/opt/scala3-${SCALA_VERSION}" /opt/scala \
    && rm /tmp/scala.tar.gz

ENV PATH="/opt/scala/bin:${PATH}" \
    SCALA_HOME="/opt/scala"

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN scala --version && echo "Scala runtime OK"
