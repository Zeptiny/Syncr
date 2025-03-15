from huey import crontab
from huey.contrib.djhuey import periodic_task

@periodic_task(crontab(minute='*/1'))
def every_min():
    print('LETS GOOOOOO')