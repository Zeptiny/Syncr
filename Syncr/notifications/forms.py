from django import forms
from . models import Contact

class ContactForm(forms.ModelForm):
    class Meta:
        model = Contact
        fields = ['name', 'email', 'discord_webhook']
        labels = {
            'name': 'Contact Name',
            'email': 'Email Address',
            'discord_webhook': 'Discord Webhook URL'
        }
        widgets = {
            'name': forms.TextInput(attrs={
                'placeholder': 'Enter contact name',
                'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'
                }),
            'email': forms.EmailInput(attrs={
                'placeholder': 'Enter email address',
                'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'
                }),
            'discord_webhook': forms.URLInput(attrs={
                'placeholder': 'Enter Discord webhook URL',
                'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'
                })
        }