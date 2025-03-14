from django import forms
from . import models

class jobCreateForm(forms.Form):
    type = forms.ChoiceField(choices=[
        ('sync/copy', 'copy')
        ])

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobCreateForm, self).__init__(*args, **kwargs)
        self.fields['srcFs'] = forms.ModelChoiceField(queryset=models.Remote.objects.filter(user=self.request.user))
        self.fields['dstFs'] = forms.ModelChoiceField(queryset=models.Remote.objects.filter(user=self.request.user))