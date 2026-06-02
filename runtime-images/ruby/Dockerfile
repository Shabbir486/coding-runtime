FROM ruby:3.3-alpine

# Install build dependencies and common gems
RUN apk add --no-cache \
    build-base \
    ca-certificates \
    tzdata \
    libffi-dev \
    yaml-dev

# Install common Ruby gems
RUN gem install --no-document \
    bundler:2.5.22 \
    json:2.7.2 \
    csv:3.3.0 \
    minitest:5.25.1 \
    rspec:3.13.0 \
    && gem cleanup

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

ENV RUBY_THREAD_VM_STACK_SIZE=1048576 \
    RUBY_FIBER_VM_STACK_SIZE=131072 \
    GEM_HOME=/sandbox/.gems \
    GEM_PATH=/sandbox/.gems:/usr/local/lib/ruby/gems/3.3.0

RUN mkdir -p /sandbox/.gems && \
    chown -R sandbox:nobody /sandbox

USER sandbox

# Verify toolchain
RUN ruby --version && echo "Ruby 3.3 runtime OK"
