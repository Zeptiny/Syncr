# Stage 1: Base build stage
FROM python:3.13-slim AS builder
 
WORKDIR /app
 
ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONUNBUFFERED=1 
 
RUN pip install --upgrade pip 
 
COPY requirements.txt /app/
RUN pip install --no-cache-dir -r requirements.txt
 
# Stage 2: Production stage
FROM python:3.13-slim
 
RUN useradd -m -r appuser && \
   mkdir /app && \
   chown -R appuser /app
 
# Copy the Python dependencies from the builder stage
COPY --from=builder /usr/local/lib/python3.13/site-packages/ /usr/local/lib/python3.13/site-packages/
COPY --from=builder /usr/local/bin/ /usr/local/bin/
 
WORKDIR /app
 
# Copy application code
COPY --chown=appuser:appuser ./Syncr .
 
# Collect static files
RUN python manage.py collectstatic --noinput
 
ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONUNBUFFERED=1 
 
# Switch to non-root user
USER appuser
 
EXPOSE 8000 
 
# Create an entrypoint script to handle migrations and starting gunicorn
COPY --chown=appuser:appuser ./docker-entrypoint.sh /app/
RUN chmod +x /app/docker-entrypoint.sh

ENTRYPOINT ["/app/docker-entrypoint.sh"]