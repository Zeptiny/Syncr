# Generated by Django 5.1.7 on 2025-03-22 21:42

from django.db import migrations, models


class Migration(migrations.Migration):

    dependencies = [
        ('sync', '0020_alter_jobrunstatistics_transferspeed_and_more'),
    ]

    operations = [
        migrations.AddField(
            model_name='jobrunstatistics',
            name='checks',
            field=models.FloatField(default=0),
            preserve_default=False,
        ),
    ]
