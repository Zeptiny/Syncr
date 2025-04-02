from .models import Notification
import requests

def send_notification(user, url, message):
    Notification.objects.create(
        user=user, 
        url=url,
        message=message
        )
    # TO-DO
    # Add webhook
    # Add email