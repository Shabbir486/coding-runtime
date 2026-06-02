FROM r-base:4.4.1

# Install system dependencies required for common R packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    libssl-dev \
    libcurl4-openssl-dev \
    libxml2-dev \
    libfontconfig1-dev \
    libharfbuzz-dev \
    libfribidi-dev \
    libfreetype6-dev \
    libpng-dev \
    libtiff5-dev \
    libjpeg-dev \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install commonly used R packages
RUN Rscript -e "options(repos = c(CRAN = 'https://cran.r-project.org')); \
    install.packages(c( \
        'data.table', \
        'dplyr', \
        'tidyr', \
        'ggplot2', \
        'jsonlite', \
        'httr', \
        'stringr', \
        'lubridate', \
        'purrr', \
        'readr' \
    ), dependencies = TRUE, quiet = TRUE)"

# Create non-root sandbox user
RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox
RUN mkdir -p /sandbox && chown sandbox:sandbox /sandbox

WORKDIR /sandbox

# R_BATCH is set internally by `R CMD BATCH`; setting it manually as an env
# var puts Rscript into batch-output mode which echoes each input line with a
# `>` prompt and appends `proc.time()` timing summary to stdout. Leave it
# unset so Rscript produces clean output.
ENV R_LIBS_USER=/sandbox/.R/library \
    R_PROFILE_USER=/dev/null \
    R_ENVIRON_USER=/dev/null \
    R_HISTFILE=/dev/null

RUN mkdir -p /sandbox/.R/library && \
    chown -R sandbox:sandbox /sandbox/.R

USER sandbox

# Verify toolchain
RUN Rscript -e "cat('R version:', R.Version()\$version.string, '\n'); library(ggplot2); cat('R runtime OK\n')"
