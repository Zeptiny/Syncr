from django.contrib import admin
from . import models

class jobAdmin(admin.ModelAdmin):
    list_display = ['id', 'user', 'group', 'rcloneId', 'startTime', 'duration', 'endTime', 'error', 'finished', 'success', 'output', 'elapsedTime', 'bytes', 'checks', 'deletedDirs', 'deletes', 'errors', 'fatalError', 'renames', 'retryError', 'serverSideCopies', 'serverSideCopyBytes', 'serverSideMoveBytes', 'serverSideMoves', 'speed', 'totalBytes', 'totalChecks', 'totalTransfers', 'transferTime', 'transfers']
    list_filter = ['user', 'startTime', 'endTime', 'finished', 'success', 'fatalError', 'retryError']

admin.site.register(models.Job, jobAdmin)

admin.site.register(models.Remote)