from .models import Notification
import requests

def send_notification(user, instance, url, message):
    Notification.objects.create(
        user=user, 
        url=url,
        message=message
        )
    contacts = instance.contacts.all()
    
    # Discord webhook
    for contact in contacts:
        if contact.discord_webhook:
            try:
                requests.post(contact.discord_webhook, json={
                    "content": f"Message: {message}\nURL: {url}"
                })
            except requests.exceptions.RequestException as e:
                print(f"Failed to send Discord notification: {e}")