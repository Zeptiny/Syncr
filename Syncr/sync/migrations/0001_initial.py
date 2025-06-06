# Generated by Django 5.1.7 on 2025-03-13 03:04

import django.db.models.deletion
from django.conf import settings
from django.db import migrations, models


class Migration(migrations.Migration):

    initial = True

    dependencies = [
        migrations.swappable_dependency(settings.AUTH_USER_MODEL),
    ]

    operations = [
        migrations.CreateModel(
            name='Job',
            fields=[
                ('id', models.BigAutoField(auto_created=True, primary_key=True, serialize=False, verbose_name='ID')),
                ('startTime', models.DateTimeField(auto_now_add=True)),
                ('duration', models.FloatField()),
                ('endTime', models.DateTimeField(null=True)),
                ('error', models.TextField(max_length=1023)),
                ('finished', models.BooleanField(default=False)),
                ('group', models.CharField(max_length=127)),
                ('rcloneId', models.CharField(max_length=127)),
                ('success', models.BooleanField(default=False)),
                ('elapsedTime', models.IntegerField()),
                ('bytes', models.IntegerField()),
                ('checks', models.IntegerField()),
                ('deletedDirs', models.IntegerField()),
                ('deletes', models.IntegerField()),
                ('errors', models.IntegerField()),
                ('fatalError', models.BooleanField(default=False)),
                ('renames', models.IntegerField()),
                ('retryError', models.BooleanField(default=False)),
                ('serverSideCopies', models.IntegerField()),
                ('serverSideCopyBytes', models.IntegerField()),
                ('serverSideMoveBytes', models.IntegerField()),
                ('serverSideMoves', models.IntegerField()),
                ('speed', models.FloatField()),
                ('totalBytes', models.IntegerField()),
                ('totalChecks', models.IntegerField()),
                ('totalTransfers', models.IntegerField()),
                ('transferTime', models.FloatField()),
                ('transfers', models.IntegerField()),
                ('user', models.ForeignKey(on_delete=django.db.models.deletion.CASCADE, to=settings.AUTH_USER_MODEL)),
            ],
        ),
    ]
