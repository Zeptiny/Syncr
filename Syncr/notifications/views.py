from django.shortcuts import render, redirect
from django.views import View
from .models import Notification
from django.http import HttpResponse

class markAsReadNotificationView(View):
    def post(self, request, notificationId):
        Notification.objects.filter(id=notificationId, user=request.user).update(is_read=True)
        return redirect('notifications:dropdown') # Returning the dropdown to update
    
    
class allNotificationsView(View):
    def get(self, request):
        context = {
            "notifications": Notification.objects.filter(user=request.user).order_by("-created_at"),
        }
        return render(request, "notifications/notifications.html", context)
    
    
class notificationDropdown(View):
    def get(self, request):
        context = {
            "notifications": Notification.objects.filter(user=request.user, is_read=False).order_by("-created_at")[:5],
        }
        return render(request, "notifications/notificationsDropdown.html", context)