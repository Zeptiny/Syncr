from huey import crontab
from huey.contrib.djhuey import periodic_task

from cron_validator import CronValidator
from datetime import datetime
from .models import Task

@periodic_task(crontab(minute='*/1'))
def every_min():
    tasks = Task.objects.all()
    dt = datetime.now()
    print(dt)
    for task in tasks:
        if CronValidator.parse(task.cron) is not None:
            print(f'Valid: {task.cron}')
        if CronValidator.match_datetime(task.cron, dt):
            print(f'Matched: {task.cron}')
        else:
            print(f'Unmatched: {task.cron}')