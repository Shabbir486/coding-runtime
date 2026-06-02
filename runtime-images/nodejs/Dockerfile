FROM node:22-alpine

# Install global TypeScript toolchain
RUN npm install -g --no-fund --no-audit \
    tsx@4.19.1 \
    typescript@5.6.3 \
    ts-node@10.9.2 \
    @types/node@22.7.9 \
    && npm cache clean --force

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

ENV NODE_ENV=production \
    NODE_OPTIONS="--max-old-space-size=256" \
    NPM_CONFIG_LOGLEVEL=silent \
    UV_THREADPOOL_SIZE=4

# Ensure sandbox user owns the workdir
RUN chown sandbox:nobody /sandbox

USER sandbox

# Verify runtime
RUN node --version && \
    tsc --version && \
    echo "Node.js runtime OK"
