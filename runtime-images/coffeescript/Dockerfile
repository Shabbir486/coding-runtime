FROM node:22-alpine

RUN npm install -g --no-fund --no-audit \
    coffeescript@2.7.0 \
    && npm cache clean --force

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

ENV NODE_ENV=production \
    NODE_OPTIONS="--max-old-space-size=256" \
    NPM_CONFIG_LOGLEVEL=silent

RUN chown sandbox:nobody /sandbox

USER sandbox

RUN node --version && \
    coffee --version && \
    echo "CoffeeScript runtime OK"