from django import forms
from . import models

from .settings import TASK_TYPES

class remoteCreateForm(forms.ModelForm):
    class Meta:
        model = models.Remote
        fields = ['type', 'name', 'config']
        labels = {
            'type': 'Remote Type',
            'name': 'Remote Name',
            'config': 'Remote Configuration'
        }
        
        widgets = {
            'name': forms.TextInput(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm rounded-lg focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'type': forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm rounded-lg focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        }
    def __init__(self, *args, **kwargs):
        super(remoteCreateForm, self).__init__(*args, **kwargs)
        self.fields['config'].widget = forms.HiddenInput()

class jobCreateForm(forms.Form):
    type = forms.ChoiceField(
        choices=list(TASK_TYPES.items()),
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm rounded-lg focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobCreateForm, self).__init__(*args, **kwargs)
        
        self.fields['srcFs'] = forms.ModelChoiceField(
            queryset=models.Remote.objects.filter(user=self.request.user),
            widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm rounded-lg focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        self.fields['dstFs'] = forms.ModelChoiceField(
            queryset=models.Remote.objects.filter(user=self.request.user),
            widget=forms.Select(attrs={'class': 'fbg-gray-50 border border-gray-300 text-gray-900 text-sm rounded-lg focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        

class taskCreateForm(forms.ModelForm):
    class Meta:
        model = models.Task
        fields = ['name', 'type', 'cron', 'srcFs', 'dstFs']
        widgets = {
            'cron': forms.TextInput(attrs={'placeholder': '0 */8 * * *'}),
        }
        labels = {
            'name': 'Task Name',
            'type': 'Task Type',
            'cron': 'Cron Frequency',
            'srcFs': 'Source Remote',
            'dstFs': 'Destination Remote'
        }

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(taskCreateForm, self).__init__(*args, **kwargs)
        self.fields['srcFs'].queryset = models.Remote.objects.filter(user=self.request.user)
        self.fields['dstFs'].queryset = models.Remote.objects.filter(user=self.request.user)
        
    # TO-DO
    # ENSURE THE CRON IS CORRECTLY FORMATTED