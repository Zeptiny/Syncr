from .models import Notification
import requests

def send_notification(user, message):
    Notification.objects.create(
        user=user, 
        message=message
        )
    # TO-DO
    # Add webhook
    # Add email