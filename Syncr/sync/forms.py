from django import forms
from . import models

from servers.models import Server

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
            'name': forms.TextInput(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'type': forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        }
    def __init__(self, *args, **kwargs):
        super(remoteCreateForm, self).__init__(*args, **kwargs)
        self.fields['config'].widget = forms.HiddenInput()

class jobCreateForm(forms.Form):
    type = forms.ChoiceField(
        choices=list(TASK_TYPES.items()),
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )

    server = forms.ModelChoiceField(
        queryset=Server.objects.all(),
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        required=True,
    )

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobCreateForm, self).__init__(*args, **kwargs)
        
        self.fields['srcFs'] = forms.ModelChoiceField(
            queryset=models.Remote.objects.filter(user=self.request.user),
            widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        self.fields['dstFs'] = forms.ModelChoiceField(
            queryset=models.Remote.objects.filter(user=self.request.user),
            widget=forms.Select(attrs={'class': 'fbg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        

class scheduleCreateForm(forms.ModelForm):
    class Meta:
        model = models.Schedule
        fields = ['name', 'type', 'cron', 'srcFs', 'dstFs', 'server']
        labels = {
            'name': 'Schedule Name',
            'type': 'Schedule Type',
            'cron': 'Cron Frequency',
            'srcFs': 'Source Remote',
            'dstFs': 'Destination Remote',
            'server': 'Server to run the jobs'
        }
        
        
        widgets = {
            'cron': forms.TextInput(attrs={'placeholder': '0 */8 * * *', 'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'name': forms.TextInput(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'type': forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        }
        
        server = forms.ModelChoiceField(
            queryset=Server.objects.all(),
            required=True,
        )

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(scheduleCreateForm, self).__init__(*args, **kwargs)
        
        self.fields['srcFs'] = forms.ModelChoiceField(
            queryset=models.Remote.objects.filter(user=self.request.user),
            widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        self.fields['dstFs'] = forms.ModelChoiceField(
            queryset=models.Remote.objects.filter(user=self.request.user),
            widget=forms.Select(attrs={'class': 'fbg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        
        self.fields['server'] = forms.ModelChoiceField(
            queryset=Server.objects.filter(online=True),
            widget=forms.Select(attrs={'class': 'fbg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
        )
        
    # TO-DO
    # ENSURE THE CRON IS CORRECTLY FORMATTED
    


class jobsSearchForm(forms.Form):
    search = forms.CharField(
        required=False,
        widget=forms.TextInput(attrs={'placeholder': 'Search for ID, srcFs and dstFs', 'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    type = forms.ChoiceField(
        choices=[('', 'All')] + list(TASK_TYPES.items()),
        required=False,
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    callee = forms.ChoiceField(
        required=False,
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    last_x_days = forms.ChoiceField(
        required=False,
        choices=[('', 'All'), ('1', '1'), ('7', '7'), ('30', '30'), ('365', '365')],
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    status = forms.ChoiceField(
        required=False,
        choices=[('', 'All'), ('success', 'Success'), ('failed', 'Failed'), ('running', 'Running')],
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    order_by = forms.ChoiceField(
        required=False,
        choices=[('startTime', 'Start Time'), ('endTime', 'End Time'), ('duration', 'Duration'), ('bytes', 'Size')],
        widget=forms.Select(attrs={'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobsSearchForm, self).__init__(*args, **kwargs)
        
        self.fields['callee'].choices = [('', 'All')] + [('manual', 'Manual')] + list(self.request.user.schedule_set.all().values_list('name', 'name'))