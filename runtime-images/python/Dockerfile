FROM python:3.12-slim

# Install scientific computing packages
RUN pip install --no-cache-dir \
    numpy==1.26.4 \
    pandas==2.2.3 \
    scipy==1.14.1 \
    scikit-learn==1.5.2 \
    matplotlib==3.9.2 \
    seaborn==0.13.2 \
    requests==2.32.3 \
    httpx==0.27.2 \
    pydantic==2.9.2

# Remove pip cache and unnecessary files
RUN find /usr/local/lib/python3.12 -name "*.pyc" -delete \
    && find /usr/local/lib/python3.12 -name "__pycache__" -type d -exec rm -rf {} + 2>/dev/null || true

# Create non-root sandbox user
RUN useradd -r -u 10001 -g nogroup -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox

WORKDIR /sandbox

# Set resource-friendly defaults
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONFAULTHANDLER=1 \
    MPLBACKEND=Agg \
    OMP_NUM_THREADS=1 \
    OPENBLAS_NUM_THREADS=1 \
    MKL_NUM_THREADS=1

# Ensure sandbox user owns the workdir
RUN chown sandbox:nogroup /sandbox

USER sandbox

# Verify Python works
RUN python -c "import numpy, pandas, scipy, sklearn, matplotlib, seaborn; print('Python runtime OK')"
