from django.urls import path
from django.contrib.auth.decorators import login_required
from . import views

app_name = "feedbacks"

urlpatterns = [
    path('new/', login_required(views.FeedbackNew.as_view()), name='new'),
]