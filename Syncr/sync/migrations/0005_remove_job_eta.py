# Generated by Django 5.1.7 on 2025-03-13 03:21

from django.db import migrations


class Migration(migrations.Migration):

    dependencies = [
        ('sync', '0004_alter_job_eta'),
    ]

    operations = [
        migrations.RemoveField(
            model_name='job',
            name='eta',
        ),
    ]
