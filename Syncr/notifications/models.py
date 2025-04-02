from django.db import models
from django.contrib.auth.models import User

class Notification(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE, related_name="notifications")
    message = models.TextField()
    created_at = models.DateTimeField(auto_now_add=True)
    is_read = models.BooleanField(default=False)
    url = models.URLField(null=True, blank=True)

    def __str__(self):
        return f"{self.user.username}: {self.message[:20]}..."
    
    
class Contact(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE, related_name="contacts")
    name = models.CharField(max_length=31)
    
    # Contact types:
    email = models.EmailField(null=True, blank=True)
    discord_webhook = models.URLField(null=True, blank=True)
    
    def __str__(self):
        return f"{self.name}"
