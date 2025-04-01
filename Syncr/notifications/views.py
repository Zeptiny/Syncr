from django.shortcuts import render
from django.views import View
from .models import Notification
from django.http import HttpResponse

class markAsReadNotificationView(View):
    def post(self, request, notificationId):
        Notification.objects.filter(id=notificationId, user=request.user).update(is_read=True)
        return HttpResponse(content="", status=200) # Returning an empty response to replace the notification div with nothing
    
    
class allNotificationsView(View):
    def get(self, request):
        context = {
            "notifications": Notification.objects.filter(user=request.user).order_by("-created_at"),
        }
        return render(request, "notifications/notifications.html", context)