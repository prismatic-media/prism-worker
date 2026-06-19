# Use a minimal and secure runtime image
FROM alpine:3.21

# Install runtime dependencies: ffmpeg for media transcoding and ca-certificates for secure HTTP requests
RUN apk add --no-cache ffmpeg ca-certificates

# Create a non-root system user and group for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Set up app and scratch/cache directory structure
WORKDIR /app
RUN mkdir -p /app/scratch && chown -R appuser:appgroup /app

# Copy the pre-built static binary into the container and ensure it is executable and owned by appuser
ARG TARGETARCH
ARG BINARY_NAME=prism-worker-linux-${TARGETARCH}
COPY --chown=appuser:appgroup --chmod=755 ${BINARY_NAME} /app/prism-worker

# Switch to the non-root user
USER appuser

# Expose default environment configuration for paths used by the worker
ENV PRISM_FFMPEG_PATH="/usr/bin/ffmpeg" \
    PRISM_FFPROBE_PATH="/usr/bin/ffprobe" \
    PRISM_SCRATCH_DIR="/app/scratch"

# Set the entrypoint to the worker binary
ENTRYPOINT ["/app/prism-worker"]
