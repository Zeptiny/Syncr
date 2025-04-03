from django.db import models
from django.contrib.auth.models import User

class Feedback(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE, related_name="feedbacks")
    created_at = models.DateTimeField(auto_now_add=True)
    message = models.TextField()
    category = models.CharField(max_length=31, choices=[
        ('bug', 'Bug Report'),
        ('feature', 'Feature Request'),
        ('other', 'Other')
    ],
        blank=False)
    admin_is_read = models.BooleanField(default=False)

    def __str__(self):
        return f"{self.user.username}: {self.message[:20]}..."
