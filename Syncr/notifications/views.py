from django.shortcuts import render, redirect
from django.views import View
from .models import Notification, Contact
from . import forms

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
    
    
    
class saveContactView(View):
    def get(self, request, contactId=None):
        
        if contactId: # If there is a schedule id
            if Contact.objects.filter(pk=contactId).exists(): # If an object with that ID exists
                schedule = Contact.objects.get(pk=contactId)
                
                if schedule.user != request.user: # Redirect to the normal creation if the user is not the owner
                    return redirect('notifications:contactNew')
                
                form = forms.ContactForm(instance=schedule)
            else: # If it doesn't exist
                return redirect('notifications:contactNew') # Redirect to without an ID
        else:
            form = forms.ContactForm()
            
        
        context = {
            "form": form,
        }
        return render(request, "notifications/contact/save.html", context)
    
    def post(self, request, contactId=None):
        
        if contactId: # If there is a schedule id
            if Contact.objects.filter(pk=contactId).exists(): # If an object with that ID exists
                schedule = Contact.objects.get(pk=contactId)
                if schedule.user != request.user: # Redirect to the normal creation if the user is not the owner
                    return redirect('notifications:contactNew')

                form = forms.ContactForm(request.POST, instance=schedule)            
            else: # If it doesn't exist
                return redirect('notifications:contactNew') # Redirect to without an ID
        else:
            form = forms.ContactForm(request.POST)
        
        
        if form.is_valid():
            contact = form.save(commit=False)
            contact.user = request.user
            contact.save()
            return redirect('notifications:contactList')
        
        context = {
            "form": form,
        }
        return render(request, "notifications/contact/save.html", context)
    
    
    
class contactListView(View):
    def get(self, request):
        context = {
            "contacts": Contact.objects.filter(user=request.user),
        }
        return render(request, "notifications/contact/list.html", context)