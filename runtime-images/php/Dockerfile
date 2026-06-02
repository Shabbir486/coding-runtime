FROM php:8.3-cli-alpine

# Upstream php:8.3-cli-alpine is multi-arch already. The local wrapper adds
# the sandbox user and a handful of commonly-needed extensions so user scripts
# don't fail with "class not found".
#
# Two-step apk pattern: runtime shared libraries (libzip, oniguruma) are
# installed as a permanent layer, and -dev headers go into a temporary
# .build-deps group that gets deleted after `docker-php-ext-install`. If you
# install everything together and then `apk del libzip-dev`, the cascade
# removes libzip itself too, which triggers
# "Unable to load dynamic library 'zip' (libzip.so: not found)" at startup.
RUN apk add --no-cache \
        libzip \
        oniguruma \
    && apk add --no-cache --virtual .build-deps \
        $PHPIZE_DEPS \
        oniguruma-dev \
        libzip-dev \
    && docker-php-ext-install \
        mbstring \
        bcmath \
        zip \
        pdo \
    && apk del .build-deps

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN php --version && echo "PHP runtime OK"
