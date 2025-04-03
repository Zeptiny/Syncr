from django.shortcuts import render, redirect
from django.views import View
from django.contrib import messages
from .forms import FeedbackForm

class FeedbackNew(View):
    def get(self, request):
        form = FeedbackForm()
        return render(request, 'feedbacks/new.html', {'form': form})

    def post(self, request):
        form = FeedbackForm(request.POST)
        
        if not form.is_valid():
            return render(request, self.template_name, {'form': form})
        
        # If everything is good
        feedback = form.save(commit=False)
        feedback.user = request.user
        feedback.save()
        
        messages.add_message(request, messages.SUCCESS, f"Thank you for your feedback!")
        
        return redirect('sync:index')
        
