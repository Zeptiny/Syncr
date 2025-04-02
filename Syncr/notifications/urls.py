from django.urls import path
from django.contrib.auth.decorators import login_required
from . import views

app_name = "notifications"

urlpatterns = [
    path('', login_required(views.allNotificationsView.as_view()), name='notifications'),
    path('markAsRead/<int:notificationId>', login_required(views.markAsReadNotificationView.as_view()), name='markAsRead'),
    
    
    path('dropdown/', login_required(views.notificationDropdown.as_view()), name='dropdown'),
    
    
    # Contact
    path('contact/list/', login_required(views.contactListView.as_view()), name='contactList'),
    path('contact/new/', login_required(views.saveContactView.as_view()), name='contactNew'),
    path('contact/edit/<int:contactId>', login_required(views.saveContactView.as_view()), name='contactEdit'),
]