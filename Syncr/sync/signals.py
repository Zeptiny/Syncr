from django.db.models.signals import post_save
from django.dispatch import receiver
from django.urls import reverse
from .models import Job
from notifications.utils import send_notification

#
# Notifications signals below
#

@receiver(post_save, sender=Job)
def send_job_failure_notification(sender, instance, created, **kwargs):
    if not created and instance.finished and not instance.success:
        send_notification(
            user = instance.user,
            instance = instance,
            url = reverse("sync:detail", args=[instance.id]),
            message = (
                f"Job {instance.id}"
                + (f" from schedule {instance.schedule.name}" if instance.schedule is not None else "")
                + " has failed."
            )
        )