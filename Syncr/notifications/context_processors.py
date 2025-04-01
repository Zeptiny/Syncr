def unread_notifications(request):
    if request.user.is_authenticated:
        return {
            "unread_notifications": request.user.notifications.filter(is_read=False)
        }
    return {}
