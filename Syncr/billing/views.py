from datetime import datetime, timedelta
from django.db.models import Count, Sum, Q
from django.db.models.functions import TruncMonth
from django.shortcuts import render
from django.views import View
from sync.models import Job

class IndexView(View):
    def get(self, request):
        last_year = datetime.now() - timedelta(days=365)
        # Filter jobs created in the last 12 months and group by month
        monthly_usage = (
            Job.objects.filter(endTime__gte=last_year, user=request.user)
            .annotate(month=TruncMonth('endTime'))  # Group by month
            .values('month')  # Select the month
            .annotate(
                total_jobs=Count('id'),
                total_failed_jobs=Count('id', filter=Q(success=False)),
                total_bandwidth=Sum('bytes'),
                total_ss_copy_bandwidth=Sum('serverSideCopyBytes'),
                total_ss_move_bandwidth=Sum('serverSideMoveBytes')
            )
            .order_by('month')
        )

        # Convert everything to Gigabytes
        for entry in monthly_usage:
            entry['total_bandwidth'] = (entry['total_bandwidth'] or 0) / 1_000_000_000
            entry['total_ss_copy_bandwidth'] = (entry['total_ss_copy_bandwidth'] or 0) / 1_000_000_000
            entry['total_ss_move_bandwidth'] = (entry['total_ss_move_bandwidth'] or 0) / 1_000_000_000

        context = {
            'monthly_usage': monthly_usage,
        }

        return render(request, 'billing/index.html', context)