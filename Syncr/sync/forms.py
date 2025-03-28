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
            'name': forms.TextInput(attrs={'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'type': forms.Select(attrs={
                'class': 'bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        }

    def __init__(self, *args, **kwargs):
        super(remoteCreateForm, self).__init__(*args, **kwargs)
        # Remove the empty label by overriding choices
        self.fields['type'].choices = [choice for choice in self.fields['type'].choices if choice[0] != '']  # Remove empty choice
        self.fields['config'].widget = forms.HiddenInput()



class remoteWidget(forms.Select):
    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request', None)  # Accept the request object
        super().__init__(*args, **kwargs)
        
        # Fetch only the Remote and Union objects owned by the current user
        if self.request:
            self.remotes = models.Remote.objects.filter(user=self.request.user)
            self.unions = models.Union.objects.filter(user=self.request.user)
        else:
            self.remotes = models.Remote.objects.none()
            self.unions = models.Union.objects.none()

    def create_option(self, name, value, label, selected, index, subindex=None, attrs=None):
        option = super().create_option(name, value, label, selected, index, subindex, attrs)

        # Parse the value to determine if it's a Remote or Union
        if value:
            value_type, value_id = value.split(':')
            if value_type == 'remote':
                remote = next((r for r in self.remotes if str(r.id) == value_id), None)
                if remote:
                    option['attrs']['type'] = remote.type
                    option['attrs']['name'] = remote.name
            elif value_type == 'union':
                union = next((u for u in self.unions if str(u.id) == value_id), None)
                if union:
                    option['attrs']['type'] = 'union'
                    option['attrs']['name'] = union.name

        return option
    
class serverWidget(forms.Select):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.servers = Server.objects.all()
        
    def create_option(self, name, value, label, selected, index, subindex=None, attrs=None):
        option = super().create_option(name, value, label, selected, index, subindex, attrs)
        server = next((s for s in self.servers if s.id == value), None)
        if server:
            option['attrs']['server'] = server
        return option

class TaskTypeWidget(forms.Select):
    def create_option(self, name, value, label, selected, index, subindex=None, attrs=None):
        option = super().create_option(name, value, label, selected, index, subindex, attrs)
        # Append the description from TASK_TYPES to the option's attributes
        if value in TASK_TYPES:
            option['attrs']['description'] = TASK_TYPES[value]['description']
        return option
    
class jobCreateForm(forms.Form):
    srcFs = forms.ChoiceField(
        required=True,
        widget=remoteWidget,
    )
    dstFs = forms.ChoiceField(
        required=True,
        widget=remoteWidget,
    )
    
    type = forms.ChoiceField(
        choices=[(key, value['display']) for key, value in TASK_TYPES.items()],
        widget=TaskTypeWidget()
    )

    server = forms.ModelChoiceField(
        queryset=Server.objects.all(),
        widget=serverWidget,
        required=True,
        empty_label=None  # Remove the default "None" choice
    )
    
    dstFsPath = forms.CharField(
        initial="/",
        widget=forms.TextInput(attrs={'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    srcFsPath = forms.CharField(
        initial="/",
        widget=forms.TextInput(attrs={'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobCreateForm, self).__init__(*args, **kwargs)
        
        # Combine Remote and Union objects into choices
        remotes = models.Remote.objects.filter(user=self.request.user)
        unions = models.Union.objects.filter(user=self.request.user)
        
        self.fields['srcFs'] = forms.ChoiceField(
            choices=[
                (f"remote:{remote.id}", f"Remote: {remote.name}") for remote in remotes
            ] + [
                (f"union:{union.id}", f"Union: {union.name}") for union in unions
            ],
            required=True,
            widget=remoteWidget(request=self.request)  # Pass the request object
        )

        self.fields['dstFs'] = forms.ChoiceField(
            choices=[
                (f"remote:{remote.id}", f"Remote: {remote.name}") for remote in remotes
            ] + [
                (f"union:{union.id}", f"Union: {union.name}") for union in unions
            ],
            required=True,
            widget=remoteWidget(request=self.request)  # Pass the request object
        )
        

class scheduleCreateForm(forms.ModelForm):
    srcFs = forms.ChoiceField(
        required=True,
        widget=remoteWidget,
    )
    dstFs = forms.ChoiceField(
        required=True,
        widget=remoteWidget,
    )
    
    class Meta:
        model = models.Schedule
        fields = ['name', 'type', 'cron', 'srcFsPath', 'dstFsPath', 'server']
        labels = {
            'name': 'Schedule Name',
            'type': 'Schedule Type',
            'cron': 'Cron Frequency',
            'srcFsPath': 'Source Remote Path',
            'dstFsPath': 'Destination Remote Path',
            'server': 'Server to run the jobs'
        }
        
        
        widgets = {
            'cron': forms.TextInput(attrs={
                'placeholder': '0 */8 * * *', 
                'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500',
                'autocomplete': 'off',
                'spellcheck': 'false',
                'aria-autocomplete': 'none',
                }),
            'name': forms.TextInput(attrs={'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'dstFsPath': forms.TextInput(attrs={'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
            'srcFsPath': forms.TextInput(attrs={'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        }

    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(scheduleCreateForm, self).__init__(*args, **kwargs)
        
        # Combine Remote and Union objects into choices
        remotes = models.Remote.objects.filter(user=self.request.user)
        unions = models.Union.objects.filter(user=self.request.user)
        
        self.fields['srcFs'] = forms.ChoiceField(
            choices=[
                (f"remote:{remote.id}", f"Remote: {remote.name}") for remote in remotes
            ] + [
                (f"union:{union.id}", f"Union: {union.name}") for union in unions
            ],
            required=True,
            widget=remoteWidget(request=self.request)  # Pass the request object
        )

        self.fields['dstFs'] = forms.ChoiceField(
            choices=[
                (f"remote:{remote.id}", f"Remote: {remote.name}") for remote in remotes
            ] + [
                (f"union:{union.id}", f"Union: {union.name}") for union in unions
            ],
            required=True,
            widget=remoteWidget(request=self.request)  # Pass the request object
        )
        
        self.fields['server'] = forms.ModelChoiceField(
            queryset=Server.objects.all(),
            required=True,
            widget=serverWidget,
            empty_label=None  # Remove the default "None" choice
        )
        
        self.fields['type'] = forms.ChoiceField(
            choices=[(key, value['display']) for key, value in TASK_TYPES.items()],
            widget=TaskTypeWidget()
        )
        
    # TO-DO
    # ENSURE THE CRON IS CORRECTLY FORMATTED

class unionCreateForm(forms.ModelForm):
    class Meta:
        model = models.Union
        fields = ['name', 'remotes']
        labels = {
            'name': 'Union Name',
            'remotes': 'Union Remotes'
        }
        
        widgets = {
            'name': forms.TextInput(attrs={'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'}),
        }
        
    def __init__ (self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(unionCreateForm, self).__init__(*args, **kwargs)
        
        self.fields['remotes'].queryset = models.Remote.objects.filter(user=self.request.user)

# This form has all the generic copy options that can be used in the copy/sync/move tasks
# https://rclone.org/commands/rclone_move/
# https://rclone.org/commands/rclone_sync/
# https://rclone.org/commands/rclone_copy/
class genericCopyOptionsForm(forms.Form):
    CheckFirst = forms.BooleanField(
        required=False,
        initial=False,
        help_text="Do all the checks before starting the transfer",
        widget=forms.CheckboxInput(attrs={
            'class': 'w-6 h-6 text-blue-600 bg-gray-100 border-gray-300 rounded-sm focus:ring-blue-500 dark:focus:ring-blue-600 dark:ring-offset-gray-800 focus:ring-2 dark:bg-gray-700 dark:border-gray-500'
        })
    )
    
    CheckSum = forms.BooleanField(
        required=False,
        initial=False,
        help_text="Check for changes with size & checksum (if available, or fallback to size only)",
        widget=forms.CheckboxInput(attrs={
            'class': 'w-6 h-6 text-blue-600 bg-gray-100 border-gray-300 rounded-sm focus:ring-blue-500 dark:focus:ring-blue-600 dark:ring-offset-gray-800 focus:ring-2 dark:bg-gray-700 dark:border-gray-500'
        })
    )
    
    Immutable = forms.BooleanField(
        required=False,
        initial=False,
        help_text="Do not modify files, fail if existing files have been modified",
        widget=forms.CheckboxInput(attrs={
            'class': 'w-6 h-6 text-blue-600 bg-gray-100 border-gray-300 rounded-sm focus:ring-blue-500 dark:focus:ring-blue-600 dark:ring-offset-gray-800 focus:ring-2 dark:bg-gray-700 dark:border-gray-500'
        })
    )
    
    Inplace = forms.BooleanField(
        required=False,
        initial=False,
        help_text="Download directly to destination file instead of atomic download to temp/rename",
        widget=forms.CheckboxInput(attrs={
            'class': 'w-6 h-6 text-blue-600 bg-gray-100 border-gray-300 rounded-sm focus:ring-blue-500 dark:focus:ring-blue-600 dark:ring-offset-gray-800 focus:ring-2 dark:bg-gray-700 dark:border-gray-500'
        })
    )
    
    PartialSuffix = forms.CharField(
        required=False,
        initial=".partial",
        help_text="Add partial-suffix to temporary file name when inplace is not used",
        widget=forms.TextInput(attrs={
            'class': 'bg-gray-50 rounded-lg border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'
        })
    )
    
    DryRun = forms.BooleanField(
        required=False,
        initial=False,
        help_text="Do a trial run with no permanent changes",
        widget=forms.CheckboxInput(attrs={
            'class': 'w-6 h-6 text-blue-600 bg-gray-100 border-gray-300 rounded-sm focus:ring-blue-500 dark:focus:ring-blue-600 dark:ring-offset-gray-800 focus:ring-2 dark:bg-gray-700 dark:border-gray-500'
        })
    )
    
    

class jobsSearchForm(forms.Form):
    search = forms.CharField(
        required=False,
        widget=forms.TextInput(attrs={'placeholder': 'Search for ID, srcFs and dstFs', 'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    type = forms.ChoiceField(
        choices=[(key, value['display']) for key, value in TASK_TYPES.items()],
        required=False,
        widget=forms.Select(attrs={'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    callee = forms.ChoiceField(
        required=False,
        widget=forms.Select(attrs={'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    last_x_days = forms.ChoiceField(
        required=False,
        choices=[('', 'All'), ('1', '1'), ('7', '7'), ('30', '30'), ('365', '365')],
        widget=forms.Select(attrs={'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    status = forms.ChoiceField(
        required=False,
        choices=[('', 'All'), ('success', 'Success'), ('failed', 'Failed'), ('running', 'Running')],
        widget=forms.Select(attrs={'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    order_by = forms.ChoiceField(
        required=False,
        choices=[('startTime', 'Start Time'), ('endTime', 'End Time'), ('duration', 'Duration'), ('bytes', 'Size')],
        widget=forms.Select(attrs={'class': 'rounded-lg bg-gray-50 border border-gray-300 text-gray-900 text-sm focus:ring-blue-500 focus:border-blue-500 block w-full p-2.5 dark:bg-gray-700 dark:border-gray-600 dark:placeholder-gray-400 dark:text-white dark:focus:ring-blue-500 dark:focus:border-blue-500'})
    )
    
    def __init__(self, *args, **kwargs):
        self.request = kwargs.pop('request')
        super(jobsSearchForm, self).__init__(*args, **kwargs)
        
        self.fields['callee'].choices = [('', 'All')] + [('manual', 'Manual')] + list(self.request.user.schedules.values_list('name', 'name'))