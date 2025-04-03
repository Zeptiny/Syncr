from django import forms
from .models import Feedback

class FeedbackForm(forms.ModelForm):
    class Meta:
        model = Feedback
        fields = ['message', 'category']
        widgets = {
            'message': forms.Textarea(attrs={
                'rows': 4, 
                'cols': 40,
                'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'category': forms.Select(),
        }
        labels = {
            'message': 'Your Feedback',
            'category': 'Category',
        }
        
    # Remove the fucking blank option
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.fields['category'].choices = [(k, v) for k, v in self.fields['category'].choices if k]
