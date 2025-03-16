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
    def __init__(self, *args, **kwargs):
        super(remoteCreateForm, self).__init__(*args, **kwargs)
        self.fields['config'].widget = forms.HiddenInput()

class jobCreateForm(forms.Form):
    type = forms.ChoiceField(choices=
        list(TASK_TYPES.items())
        )

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobCreateForm, self).__init__(*args, **kwargs)
        self.fields['srcFs'] = forms.ModelChoiceField(queryset=models.Remote.objects.filter(user=self.request.user))
        self.fields['dstFs'] = forms.ModelChoiceField(queryset=models.Remote.objects.filter(user=self.request.user))
        

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