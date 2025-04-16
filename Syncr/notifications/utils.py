from .models import Notification
from django.core.mail import send_mail
import requests

def send_notification(user, instance, url, message):
    Notification.objects.create(
        user=user, 
        url=url,
        message=message
        )
    contacts = instance.contacts.all()
    
    for contact in contacts:
        
        if contact.discord_webhook:
            try:
                requests.post(contact.discord_webhook, json={
                    "content": f"Message: {message}\nURL: {url}"
                })
            except requests.exceptions.RequestException as e:
                print(f"Failed to send Discord notification: {e}")
        
        if contact.email:
            try:
                # Is it ugly? Possibly
                send_mail(
                    subject=f"Notification: {instance._meta.verbose_name}",
                    message=f"Message: {message}\nURL: {url}",
                    from_email="TO-DO-THING",
                    recipient_list=[contact.email],
                    fail_silently=False,
                )
            except Exception as e:
                print(f"Failed to send Email: {e}")