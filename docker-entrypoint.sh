#!/bin/bash

# Apply database migrations
echo "Applying database migrations..."
python manage.py migrate

# Start Gunicorn
echo "Starting Gunicorn server..."
exec gunicorn --bind 0.0.0.0:8000 --workers 3 Syncr.wsgi:application