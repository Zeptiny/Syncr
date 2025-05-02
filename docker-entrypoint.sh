#!/bin/bash
set -e

# Check if a command was specified
if [ "$1" = "huey" ]; then
    echo "Starting Huey worker..."
    exec python manage.py run_huey
elif [ "$1" = "web" ] || [ -z "$1" ]; then
    echo "Applying database migrations..."
    python manage.py migrate

    echo "Starting Gunicorn server..."
    exec gunicorn --bind 0.0.0.0:8000 --workers 3 Syncr.wsgi:application
else
    # Execute whatever command was passed
    exec "$@"
fi